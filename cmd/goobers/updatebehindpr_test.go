package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

const (
	updateBehindPRToken     = "pr-write-token"
	updateBehindIssuesToken = "issues-write-token"
)

type updateBehindServer struct {
	mergeable           *bool
	labels              []string
	comments            []map[string]interface{}
	checkState          string
	updateCalls         int
	updateStatus        int
	current             bool
	failLabelDelete     bool
	includeUnselected   bool
	includeEarlierCrown bool
	unselectedCount     int
	graphQLCalls        int
}

func (s *updateBehindServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	const (
		prefix  = "/repos/your-org/your-repo"
		headSHA = "head-sha"
		baseSHA = "live-base-sha"
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		writeFakeJSON(w, map[string]string{"login": "merge-review-bot"})
	})
	mux.HandleFunc(prefix+"/pulls", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "want GET", http.StatusMethodNotAllowed)
			return
		}
		prs := []map[string]interface{}{{
			"number": 55, "state": "open", "html_url": "https://github.test/pulls/55",
			"head":   map[string]string{"ref": "goobers/implementation/run-55", "sha": headSHA},
			"base":   map[string]string{"ref": "main", "sha": "opening-base-sha"},
			"labels": labelsJSON(s.labels),
		}}
		if s.includeEarlierCrown {
			prs = append([]map[string]interface{}{{
				"number": 54, "state": "open", "html_url": "https://github.test/pulls/54",
				"head": map[string]string{"ref": "goobers/implementation/run-54", "sha": "earlier-passing-sha"},
				"base": map[string]string{"ref": "main", "sha": "opening-base-sha"},
			}}, prs...)
			prs = append(prs, map[string]interface{}{
				"number": 1055, "state": "open", "html_url": "https://github.test/pulls/1055",
				"head":   map[string]string{"ref": "goobers/implementation/run-1055", "sha": "parked-dependent-sha"},
				"base":   map[string]string{"ref": "main", "sha": "opening-base-sha"},
				"labels": labelsJSON([]string{blockedOnSiblingLabel}),
			})
		}
		unselectedCount := s.unselectedCount
		if s.includeUnselected && unselectedCount == 0 {
			unselectedCount = 1
		}
		for i := 0; i < unselectedCount; i++ {
			prs = append(prs, map[string]interface{}{
				"number": 56 + i, "state": "open", "html_url": fmt.Sprintf("https://github.test/pulls/%d", 56+i),
				"head": map[string]string{"ref": fmt.Sprintf("goobers/implementation/run-%d", 56+i), "sha": fmt.Sprintf("unselected-head-sha-%d", i)},
				"base": map[string]string{"ref": "main", "sha": "opening-base-sha"},
			})
		}
		writeFakeJSON(w, prs)
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		state := "SUCCESS"
		if s.checkState == "failure" {
			state = "FAILURE"
		}
		serveBulkCheckStates(t, w, r, &s.graphQLCalls, map[string]string{headSHA: state})
	})
	mux.HandleFunc(prefix+"/commits/"+headSHA+"/status", func(w http.ResponseWriter, _ *http.Request) {
		state := s.checkState
		if state == "" {
			state = "success"
		}
		writeFakeJSON(w, map[string]interface{}{
			"state": state,
			"statuses": []map[string]string{{
				"context": "required-ci",
				"state":   state,
			}},
		})
	})
	mux.HandleFunc(prefix+"/commits/"+headSHA+"/check-runs", func(w http.ResponseWriter, _ *http.Request) {
		writeFakeJSON(w, map[string]interface{}{"check_runs": []interface{}{}})
	})
	mux.HandleFunc(prefix+"/commits/earlier-passing-sha/status", func(w http.ResponseWriter, _ *http.Request) {
		writeFakeJSON(w, map[string]interface{}{"state": "success", "statuses": []interface{}{}})
	})
	mux.HandleFunc(prefix+"/commits/earlier-passing-sha/check-runs", func(w http.ResponseWriter, _ *http.Request) {
		writeFakeJSON(w, map[string]interface{}{"check_runs": []interface{}{}})
	})
	mux.HandleFunc(prefix+"/commits/unselected-head-sha-", func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("initial pull-request list resolved check state for unselected PR: %s", r.URL.Path)
	})
	mux.HandleFunc(prefix+"/git/ref/heads/main", func(w http.ResponseWriter, _ *http.Request) {
		writeFakeJSON(w, map[string]interface{}{"object": map[string]string{"sha": baseSHA}})
	})
	mux.HandleFunc(prefix+"/compare/"+baseSHA+"..."+headSHA, func(w http.ResponseWriter, _ *http.Request) {
		mergeBaseSHA := "opening-base-sha"
		if s.current {
			mergeBaseSHA = baseSHA
		}
		writeFakeJSON(w, map[string]interface{}{
			"merge_base_commit": map[string]string{"sha": mergeBaseSHA},
			"files":             []interface{}{},
		})
	})
	mux.HandleFunc(prefix+"/pulls/55/update-branch", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "want PUT", http.StatusMethodNotAllowed)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode update body: %v", err)
		}
		if body["expected_head_sha"] != headSHA {
			t.Fatalf("expected_head_sha = %q, want %q", body["expected_head_sha"], headSHA)
		}
		s.updateCalls++
		status := s.updateStatus
		if status == 0 {
			status = http.StatusAccepted
		}
		w.WriteHeader(status)
		if status == http.StatusAccepted {
			s.current = true
			_, _ = w.Write([]byte(`{"message":"Updating pull request branch.","url":"https://github.test/pulls/55"}`))
			return
		}
		_, _ = w.Write([]byte(`{"message":"expected head SHA did not match"}`))
	})
	mux.HandleFunc(prefix+"/pulls/55", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "want GET", http.StatusMethodNotAllowed)
			return
		}
		writeFakeJSON(w, map[string]interface{}{"number": 55, "mergeable": s.mergeable})
	})
	mux.HandleFunc(prefix+"/issues/55/comments", func(w http.ResponseWriter, _ *http.Request) {
		writeFakeJSON(w, s.comments)
	})
	if s.includeEarlierCrown {
		mux.HandleFunc(prefix+"/issues/1055/comments", func(w http.ResponseWriter, _ *http.Request) {
			writeFakeJSON(w, []map[string]interface{}{{
				"id": 1055, "body": blockedOnSiblingCommentFor(t, 54),
			}})
		})
		mux.HandleFunc(prefix+"/issues/54", func(w http.ResponseWriter, _ *http.Request) {
			writeFakeJSON(w, map[string]interface{}{
				"number": 54, "state": "open", "html_url": "https://github.test/pulls/54", "labels": []interface{}{},
			})
		})
	}
	mux.HandleFunc(prefix+"/issues/55/labels/"+needsRemediationLabel, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "want DELETE", http.StatusMethodNotAllowed)
			return
		}
		if s.failLabelDelete {
			http.Error(w, "temporary label failure", http.StatusServiceUnavailable)
			return
		}
		next := s.labels[:0]
		for _, label := range s.labels {
			if label != needsRemediationLabel {
				next = append(next, label)
			}
		}
		s.labels = next
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc(prefix+"/issues/55", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "want GET", http.StatusMethodNotAllowed)
			return
		}
		writeFakeJSON(w, map[string]interface{}{
			"number": 55, "title": "test PR", "state": "open",
			"html_url": "https://github.test/pulls/55", "labels": labelsJSON(s.labels),
		})
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantToken := updateBehindPRToken
		if r.URL.Path == prefix+"/issues/55" ||
			(r.Method == http.MethodDelete && r.URL.Path == prefix+"/issues/55/labels/"+needsRemediationLabel) {
			wantToken = updateBehindIssuesToken
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+wantToken {
			t.Errorf("%s %s Authorization = %q, want bearer token for the operation's declared capability", r.Method, r.URL.Path, got)
			http.Error(w, "wrong token", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

func setupUpdateBehindPRTest(t *testing.T, state *updateBehindServer) (root, workspace string) {
	t.Helper()
	server := state.start(t)
	previous := newGitHubProvider
	newGitHubProvider = mergePRTestServer{url: server.URL}.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = previous })

	root = initDemo(t)
	t.Setenv("GOOBERS_RUN_ID", "run-720")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderGitHub))
	t.Setenv(executor.RepoOwnerEnvVar, "your-org")
	t.Setenv(executor.RepoNameEnvVar, "your-repo")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", updateBehindPRToken)
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", updateBehindIssuesToken)
	workspace = t.TempDir()
	t.Chdir(workspace)
	return root, workspace
}

func invokeUpdateBehindPRTest(t *testing.T, root, workspace string) (code int, stdout, stderr string, result map[string]string) {
	t.Helper()
	t.Chdir(workspace)
	code, stdout, stderr = runArgs(t, "update-behind-pr", root)
	if code != 0 {
		return code, stdout, stderr, nil
	}
	data, err := os.ReadFile(filepath.Join(workspace, "update-behind-result.json"))
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	return code, stdout, stderr, result
}

func runUpdateBehindPRTest(t *testing.T, state *updateBehindServer) (stdout, stderr string, result map[string]string) {
	t.Helper()
	root, workspace := setupUpdateBehindPRTest(t, state)
	code, stdout, stderr, result := invokeUpdateBehindPRTest(t, root, workspace)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	return stdout, stderr, result
}

func TestUpdateBehindPRUsesAPIAndClearsLabel(t *testing.T) {
	mergeable := true
	state := &updateBehindServer{
		mergeable:       &mergeable,
		labels:          []string{needsRemediationLabel, "other"},
		unselectedCount: 40,
	}
	stdout, _, result := runUpdateBehindPRTest(t, state)

	if state.updateCalls != 1 {
		t.Fatalf("update-branch calls = %d, want 1", state.updateCalls)
	}
	if state.graphQLCalls != 1 {
		t.Fatalf("bulk check-state calls = %d, want 1 for 41 PRs", state.graphQLCalls)
	}
	if strings.Join(state.labels, ",") != "other" {
		t.Fatalf("labels = %v, want needs-remediation cleared", state.labels)
	}
	if result["needsFullRemediation"] != "false" || result["selectedNumber"] != "55" {
		t.Fatalf("result = %v", result)
	}
	if !strings.Contains(stdout, "updated behind branch through GitHub API") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestUpdateBehindPRDispatchesToGitea(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/api/v1/repos/your-org/your-repo/pulls" {
			t.Errorf("request path = %q, want Gitea pulls endpoint", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "token "+updateBehindPRToken {
			t.Errorf("Authorization = %q, want Gitea token", got)
			http.Error(w, "wrong token", http.StatusUnauthorized)
			return
		}
		writeFakeJSON(w, []interface{}{})
	}))
	t.Cleanup(server.Close)

	root := initDemo(t)
	configureRemediationGitea(t, root, server.URL)
	t.Setenv("GOOBERS_RUN_ID", "run-gitea-update")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderGitea))
	t.Setenv(executor.RepoOwnerEnvVar, "your-org")
	t.Setenv(executor.RepoNameEnvVar, "your-repo")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", updateBehindPRToken)
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", updateBehindIssuesToken)
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "update-behind-pr", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if requests != 1 {
		t.Fatalf("Gitea requests = %d, want 1", requests)
	}
	if !strings.Contains(stdout, "no work") || stderr != "" {
		t.Fatalf("stdout = %q, stderr = %q, want clean no-work result", stdout, stderr)
	}
}

func TestUpdateBehindPRRoutesFailingCurrentUnlabeledPRToFullRemediation(t *testing.T) {
	state := &updateBehindServer{
		checkState:          "failure",
		current:             true,
		unselectedCount:     40,
		includeEarlierCrown: true,
	}
	stdout, _, result := runUpdateBehindPRTest(t, state)

	if state.updateCalls != 0 {
		t.Fatalf("update-branch calls = %d, want 0 for current PR", state.updateCalls)
	}
	if state.graphQLCalls != 1 {
		t.Fatalf("bulk check-state calls = %d, want 1 for 42 PRs", state.graphQLCalls)
	}
	if result["needsFullRemediation"] != "true" || result["selectedNumber"] != "55" {
		t.Fatalf("result = %v, want failing PR routed to full remediation", result)
	}
	if len(state.labels) != 0 {
		t.Fatalf("labels = %v, want unlabeled candidate unchanged", state.labels)
	}
	if !strings.Contains(stdout, "requires full remediation") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestUpdateBehindPRFailingCurrentUnlabeledHandoffReachesGatherPRContext(t *testing.T) {
	state := &updateBehindServer{
		checkState: "failure",
		current:    true,
	}
	root, workspace := setupUpdateBehindPRTest(t, state)
	code, stdout, stderr, updateResult := invokeUpdateBehindPRTest(t, root, workspace)
	if code != 0 {
		t.Fatalf("update-behind-pr: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if updateResult["needsFullRemediation"] != "true" {
		t.Fatalf("update result = %v, want full remediation", updateResult)
	}
	if state.updateCalls != 0 || len(state.labels) != 0 {
		t.Fatalf("update calls = %d, labels = %v, want current unlabeled candidate unchanged", state.updateCalls, state.labels)
	}

	const prBranch = "goobers/implementation/run-55"
	origin, _, baseSHA := initPRBranchOrigin(t, prBranch)
	work := filepath.Join(filepath.Dir(origin), "work")
	runGitT(t, work, "checkout", prBranch)
	runGitT(t, work, "merge", "--no-edit", "origin/main")
	runGitT(t, work, "push", "origin", prBranch)
	headSHA := strings.TrimSpace(runGitOutputT(t, work, "rev-parse", "HEAD"))

	gatherState := gatherPRContextServer{
		owner: "your-org", repo: "your-repo",
		prNumber: 55, head: prBranch, base: "main",
		headSHA: headSHA, baseSHA: baseSHA,
		checkState: "failure",
	}
	gatherServer := gatherState.start(t)
	newGitHubProvider = mergePRTestServer{url: gatherServer.URL}.newGitHubProvider

	mgr, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	wt, err := mgr.Create(t.Context(), worktree.CreateOptions{
		RepoURL: origin, RunID: "run-720", BaseRef: "main",
		Branch: "goobers/pr-remediation/run-720",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = wt.Remove(t.Context(), worktree.RemoveOptions{}) })

	t.Setenv(executor.InputEnvVar("selectedNumber"), updateResult["selectedNumber"])
	t.Setenv("GOOBERS_CRED_REPO_PUSH", "repo-push-token")
	t.Chdir(wt.Path)
	code, stdout, stderr = runArgs(t, "gather-pr-context", root)
	if code != 0 {
		t.Fatalf("gather-pr-context: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	data, err := os.ReadFile(filepath.Join(wt.Path, remediationBriefResultFile))
	if err != nil {
		t.Fatalf("read remediation brief: %v", err)
	}
	var brief apiv1.RemediationBrief
	if err := json.Unmarshal(data, &brief); err != nil {
		t.Fatalf("decode remediation brief: %v", err)
	}
	if brief.SelectedNumber != updateResult["selectedNumber"] ||
		brief.HasFailingCI != "true" ||
		brief.IsBehindBase {
		t.Fatalf("remediation brief = %+v, want selected failing current PR", brief)
	}
}

func TestUpdateBehindPRSkipsNeedsHuman(t *testing.T) {
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderGitHub))
	t.Setenv(executor.RepoOwnerEnvVar, "your-org")
	t.Setenv(executor.RepoNameEnvVar, "your-repo")
	state := &updateBehindServer{
		labels: []string{needsRemediationLabel, providers.LabelNeedsHuman},
	}
	root, _ := setupUpdateBehindPRTest(t, state)

	code, stdout, stderr := runArgs(t, "update-behind-pr", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if state.updateCalls != 0 {
		t.Fatalf("update-branch calls = %d, want 0", state.updateCalls)
	}
	if got := strings.Join(state.labels, ","); got != needsRemediationLabel+","+providers.LabelNeedsHuman {
		t.Fatalf("labels = %v, want unchanged", state.labels)
	}
	if !strings.Contains(stdout, "no work") || stderr != "" {
		t.Fatalf("stdout = %q, stderr = %q", stdout, stderr)
	}
}

func TestUpdateBehindPRRoutesNonTrivialCandidatesToFullRemediation(t *testing.T) {
	substantive := renderVerdictComment(apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Findings: []apiv1.Finding{{
			Class: apiv1.FindingSubstantive, Message: "fix the defect", Location: "PR #55",
		}},
	})
	tests := []struct {
		name      string
		mergeable bool
		comments  []map[string]interface{}
	}{
		{name: "conflict", mergeable: false},
		{
			name:      "substantive finding",
			mergeable: true,
			comments: []map[string]interface{}{{
				"id": 1, "user": map[string]string{"login": "merge-review-bot"},
				"body": substantive, "created_at": "2026-07-20T00:00:00Z",
			}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := &updateBehindServer{
				mergeable: &test.mergeable,
				labels:    []string{needsRemediationLabel},
				comments:  test.comments,
			}
			_, _, result := runUpdateBehindPRTest(t, state)
			if state.updateCalls != 0 {
				t.Fatalf("update-branch calls = %d, want 0", state.updateCalls)
			}
			if result["needsFullRemediation"] != "true" {
				t.Fatalf("result = %v, want full remediation", result)
			}
			if strings.Join(state.labels, ",") != needsRemediationLabel {
				t.Fatalf("labels = %v, want unchanged", state.labels)
			}
		})
	}
}

func TestUpdateBehindPRAttemptsAPIWhileMergeabilityIsUnknown(t *testing.T) {
	state := &updateBehindServer{labels: []string{needsRemediationLabel}}
	_, _, result := runUpdateBehindPRTest(t, state)

	if state.updateCalls != 1 {
		t.Fatalf("update-branch calls = %d, want 1", state.updateCalls)
	}
	if result["needsFullRemediation"] != "false" {
		t.Fatalf("result = %v, want API update to complete", result)
	}
}

func TestUpdateBehindPRDoesNotEagerlyUpdateUnelectedBehindPR(t *testing.T) {
	state := &updateBehindServer{}
	root, _ := setupUpdateBehindPRTest(t, state)

	code, stdout, stderr := runArgs(t, "update-behind-pr", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if state.updateCalls != 0 {
		t.Fatalf("update-branch calls = %d, want 0 before merge-review elects the behind-base PR", state.updateCalls)
	}
	if !strings.Contains(stdout, "no work") || stderr != "" {
		t.Fatalf("stdout = %q, stderr = %q, want clean no-work result", stdout, stderr)
	}
}

func TestUpdateBehindPRRoutesLeaseRejectionToFullRemediation(t *testing.T) {
	mergeable := true
	state := &updateBehindServer{
		mergeable:    &mergeable,
		labels:       []string{needsRemediationLabel},
		updateStatus: http.StatusUnprocessableEntity,
	}
	_, _, result := runUpdateBehindPRTest(t, state)
	if state.updateCalls != 1 {
		t.Fatalf("update-branch calls = %d, want 1", state.updateCalls)
	}
	if result["needsFullRemediation"] != "true" {
		t.Fatalf("result = %v, want full remediation after lease rejection", result)
	}
	if strings.Join(state.labels, ",") != needsRemediationLabel {
		t.Fatalf("labels = %v, want unchanged", state.labels)
	}
}

func TestUpdateBehindPRRetryAfterLabelFailureOnlyClearsLabel(t *testing.T) {
	mergeable := true
	state := &updateBehindServer{
		mergeable:       &mergeable,
		labels:          []string{needsRemediationLabel},
		failLabelDelete: true,
	}
	root, workspace := setupUpdateBehindPRTest(t, state)
	// failLabelDelete's 503 is here to prove the label cleanup is retried on
	// the NEXT run, not to time the in-request backoff ladder. Spend the
	// transient-retry budget so the first attempt fails fast instead of
	// burning 1+2+4+8 = 15s of real sleep; setupUpdateBehindPRTest's own
	// t.Cleanup still restores the original factory.
	baseFactory := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		return baseFactory(token, append(opts, providers.WithMaxTransientRetries(0))...)
	}

	code, _, _, _ := invokeUpdateBehindPRTest(t, root, workspace)
	if code != 1 {
		t.Fatalf("first attempt code = %d, want 1 after label removal failure", code)
	}
	if state.updateCalls != 1 || !state.current {
		t.Fatalf("first attempt update calls = %d, current = %v, want one accepted update", state.updateCalls, state.current)
	}
	if strings.Join(state.labels, ",") != needsRemediationLabel {
		t.Fatalf("first attempt labels = %v, want retained after failed removal", state.labels)
	}

	state.failLabelDelete = false
	code, stdout, stderr, result := invokeUpdateBehindPRTest(t, root, workspace)
	if code != 0 {
		t.Fatalf("retry code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if result["needsFullRemediation"] != "false" {
		t.Fatalf("retry result = %v, want label-cleanup completion", result)
	}
	if state.updateCalls != 1 {
		t.Fatalf("retry update-branch calls = %d, want no duplicate update", state.updateCalls)
	}
	if len(state.labels) != 0 {
		t.Fatalf("retry labels = %v, want needs-remediation cleared", state.labels)
	}
	if !strings.Contains(stdout, "branch is current") || stderr != "" {
		t.Fatalf("retry stdout = %q, stderr = %q", stdout, stderr)
	}
}
