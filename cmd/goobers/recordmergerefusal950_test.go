package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

func issueHasLabel(server *fakeGitHubServer, number int, label string) bool {
	server.mu.Lock()
	defer server.mu.Unlock()
	for _, l := range server.issues[number].labels {
		if l == label {
			return true
		}
	}
	return false
}

// TestRecordMergeRefusalDemotesAfterThreshold is #950's recorder end to end:
// consecutive merge refusals at an unchanged head accrue toward the threshold,
// and the crossing one applies goobers:merge-demoted so the election can crown a
// sibling instead.
func TestRecordMergeRefusalDemotesAfterThreshold(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addOpenPR(77, "goobers/implementation/stuck", "main", "sha-stuck", "base1", false, nil, nil)
	server.addIssue(77, "stuck lander")
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "77")
	t.Setenv("GOOBERS_INPUT_SELECTEDHEADSHA", "sha-stuck")
	t.Setenv("GOOBERS_INPUT_REASON", "base moved: verdict pinned to base1, PR is now based on base2, and that movement touches files this PR also changes")

	workDir := t.TempDir()
	t.Chdir(workDir)

	for attempt := 1; attempt <= defaultDemotionThreshold; attempt++ {
		code, stdout, stderr := runArgs(t, "record-merge-refusal", root)
		if code != 0 {
			t.Fatalf("attempt %d: code = %d, stderr = %q", attempt, code, stderr)
		}
		demoted := issueHasLabel(server, 77, mergeDemotedLabel)
		switch {
		case attempt < defaultDemotionThreshold && demoted:
			t.Fatalf("attempt %d: demoted too early (stdout=%q)", attempt, stdout)
		case attempt == defaultDemotionThreshold && !demoted:
			t.Fatalf("attempt %d: expected goobers:merge-demoted after crossing the threshold (stdout=%q)", attempt, stdout)
		}
	}
}

func TestRecordMergeRefusalKeepsTransientRefusalVisibleAndReselectable(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addOpenPR(78, "goobers/implementation/transient", "main", "sha-transient", "base1", false, nil, nil)
	server.addIssue(78, "transient refusal")
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "78")
	t.Setenv("GOOBERS_INPUT_SELECTEDHEADSHA", "sha-transient")
	const reason = "base moved after review without touching this pull request's files"
	t.Setenv("GOOBERS_INPUT_REASON", reason)
	t.Chdir(t.TempDir())

	for attempt := 1; attempt < defaultDemotionThreshold; attempt++ {
		code, _, stderr := runArgs(t, "record-merge-refusal", root)
		if code != 0 {
			t.Fatalf("attempt %d: code = %d, stderr = %q", attempt, code, stderr)
		}
	}

	server.mu.Lock()
	labels := append([]string(nil), server.issues[78].labels...)
	comments := append([]string(nil), server.issues[78].comments...)
	server.mu.Unlock()
	if len(labels) != 0 {
		t.Fatalf("labels = %v, want none before the unchanged-head threshold so the PR remains re-selectable", labels)
	}
	if len(comments) != 1 {
		t.Fatalf("comments = %d, want one sticky refusal trail rather than one comment per cycle", len(comments))
	}
	if !strings.Contains(comments[0], reason) {
		t.Fatalf("comment = %q, want merge-pr's refusal reason to reach the PR", comments[0])
	}
}

// TestRecordMergeRefusalSkipsAdvisory proves an advisory-mode "refusal" (no real
// merge attempted) never accrues toward demotion — otherwise advisory mode would
// demote every lander every cycle.
func TestRecordMergeRefusalSkipsAdvisory(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addOpenPR(80, "goobers/implementation/adv", "main", "sha-adv", "base1", false, nil, nil)
	server.addIssue(80, "advisory pr")
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "80")
	t.Setenv("GOOBERS_INPUT_SELECTEDHEADSHA", "sha-adv")
	t.Setenv("GOOBERS_INPUT_REASON", "advisory mode: no merge attempted")
	t.Setenv("GOOBERS_INPUT_DEMOTIONTHRESHOLD", "1") // would demote on the first real refusal

	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "record-merge-refusal", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "advisory") {
		t.Errorf("stdout = %q, want an advisory-mode skip message", stdout)
	}
	if issueHasLabel(server, 80, mergeDemotedLabel) {
		t.Fatal("an advisory-mode result must not demote the PR")
	}
}

func TestRecordMergeRefusalSkipsMergeReviewOptOut(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addOpenPR(82, "goobers/implementation/opted-out", "main", "sha-opted-out", "base1", false, []string{noMergeReviewLabel}, nil)
	server.addIssue(82, "opted-out pr")
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "82")
	t.Setenv("GOOBERS_INPUT_SELECTEDHEADSHA", "sha-opted-out")
	t.Setenv("GOOBERS_INPUT_REASON", mergeReviewOptOutReason)
	t.Setenv("GOOBERS_INPUT_DEMOTIONTHRESHOLD", "1")
	t.Chdir(t.TempDir())

	code, _, stderr := runArgs(t, "record-merge-refusal", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}

	server.mu.Lock()
	comments := append([]string(nil), server.issues[82].comments...)
	labels := append([]string(nil), server.issues[82].labels...)
	server.mu.Unlock()
	if len(comments) != 0 || len(labels) != 0 {
		t.Fatalf("opt-out refusal mutated PR: comments=%v issue labels=%v", comments, labels)
	}

}

// TestRecordMergeRefusalResetsOnHeadAdvance proves a refusal at a NEW head resets
// the counter — a PR whose head advanced (a remediation push) is a genuinely
// fresh attempt, not a continuation of the stuck run.
func TestRecordMergeRefusalResetsOnHeadAdvance(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	// The PR's live head is new-head, but the recorded prior refusals were at
	// old-head — so the count must reset rather than accumulate to demotion.
	server.addOpenPR(81, "goobers/implementation/moved", "main", "new-head", "base1", false, nil, nil)
	server.addIssue(81, "moved pr")
	prior, err := mergeDemotionComment(mergeDemotionState{Attempts: 2, Demoted: false, HeadSHA: "old-head"})
	if err != nil {
		t.Fatalf("mergeDemotionComment: %v", err)
	}

	server.addComment(81, prior)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "81")
	t.Setenv("GOOBERS_INPUT_SELECTEDHEADSHA", "new-head")
	t.Setenv("GOOBERS_INPUT_REASON", "base moved: touches this PR's files")

	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "record-merge-refusal", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if issueHasLabel(server, 81, mergeDemotedLabel) {
		t.Fatal("a refusal at a new head must reset the counter, not demote (prior attempts were at a different head)")
	}
	if !strings.Contains(stdout, "1/") {
		t.Errorf("stdout = %q, want the counter reset to attempt 1", stdout)
	}
}

func TestRecordMergeRefusalADOUsesADOProviderPath(t *testing.T) {
	root, repo := providerDispatchFixture(t, providers.ProviderADO)
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderADO))
	t.Setenv(executor.RepoOwnerEnvVar, repo.Owner)
	t.Setenv(executor.RepoProjectEnvVar, repo.Project)
	t.Setenv(executor.RepoNameEnvVar, repo.Name)
	t.Setenv(executor.CredentialEnvVar(string(capability.ADOPRWrite)), "ado-token")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "359")
	t.Setenv("GOOBERS_INPUT_SELECTEDHEADSHA", "head-sha")
	t.Setenv("GOOBERS_INPUT_REASON", "base moved")
	t.Chdir(t.TempDir())

	code, _, stderr := runArgs(t, "record-merge-refusal", root)
	if code != 1 {
		t.Fatalf("code = %d, want provider failure after ADO dispatch; stderr = %q", code, stderr)
	}
	if strings.Contains(stderr, "does not support merge refusal recording") {
		t.Fatalf("stderr = %q, ADO refusal path must dispatch before provider failure", stderr)
	}
}

func TestRecordMergeRefusalDispatchesADOAndRecordsComment(t *testing.T) {
	root, repo := providerDispatchFixture(t, providers.ProviderADO)
	commentsPath := "/" + repo.Owner + "/" + repo.Project + "/_apis/wit/workItems/359/comments"
	var posted bool
	mux := http.NewServeMux()
	mux.HandleFunc(commentsPath, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"comments":[]}`))
		case http.MethodPost:
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode comment request: %v", err)
			}
			if body["text"] == "" {
				t.Error("comment request has empty text")
			}
			posted = true
			_, _ = w.Write([]byte(`{"id":1,"text":"recorded"}`))
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/"+repo.Owner+"/"+repo.Project+"/_apis/wit/workitems/359", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"id":359,"rev":1,"url":"https://ado.test/workitems/359","fields":{"System.Title":"ADO refusal","System.Description":"","System.State":"Active","System.WorkItemType":"Bug","System.Tags":""}}`))
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/"+repo.Owner+"/"+repo.Project+"/_apis/wit/workitemtypes/Bug/states", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"value":[{"name":"Active","category":"InProgress"}]}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	previous := newADOProviderForStage
	newADOProviderForStage = func(_ string, routed providers.RepositoryRef) (*providers.ADOProvider, error) {
		return providers.NewADOProvider(routed.Owner, routed.Project, "ado-token", func(p *providers.ADOProvider) {
			p.BaseURL = server.URL
		}), nil
	}
	t.Cleanup(func() { newADOProviderForStage = previous })

	t.Setenv(executor.RepoProviderEnvVar, string(repo.Provider))
	t.Setenv(executor.RepoOwnerEnvVar, repo.Owner)
	t.Setenv(executor.RepoProjectEnvVar, repo.Project)
	t.Setenv(executor.RepoNameEnvVar, repo.Name)
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "359")
	t.Setenv("GOOBERS_INPUT_SELECTEDHEADSHA", "head-sha")
	t.Setenv("GOOBERS_INPUT_REASON", "base moved")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "record-merge-refusal", root)
	if code != 0 {
		t.Fatalf("record-merge-refusal: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !posted {
		t.Fatal("ADO work-item comment endpoint was not called")
	}
	if !strings.Contains(stdout, "recorded merge refusal 1/") {
		t.Fatalf("stdout = %q, want refusal recorded message", stdout)
	}
}
