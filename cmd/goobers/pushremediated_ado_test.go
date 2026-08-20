package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// adoRemediationServerState is a small stateful fake Azure DevOps REST server for
// the pr-remediation ADO stage tests (rebase-pr, push-remediated). It serves the
// single-PR detail PollPullRequest/GetPullRequest read, the PR threads that carry
// the sticky remediation-state comment (the head-SHA lease source), an empty
// blocking-policy set, and the native PR-label DELETE the stages clear
// goobers:needs-remediation through. A wit/workitems call is treated as a test
// failure: routing a PR-state write to wit/workitems is the PR-as-work-item
// wrong-object hazard the ADO branches exist to avoid.
type adoRemediationServerState struct {
	mu       sync.Mutex
	owner    string
	project  string
	name     string
	prNumber int
	prBranch string
	base     string
	headSHA  string
	baseSHA  string
	status   string
	labels   []string
	// threadComments are the bodies of one bot-authored PR thread's comments, in
	// order; the sticky remediation-state comment is seeded here so
	// ListPullRequestThreadComments surfaces it exactly as a GitHub PR comment.
	threadComments []string
	// deletedLabels records every label name cleared via the native PR-label
	// DELETE, proving the reroute of the GitHub UpdateWorkItem(ID: PR#) path.
	deletedLabels []string
	// workItemHit is set if any wit/workitems endpoint was touched — it must stay
	// false (the wrong-object hazard, remediation-wiring-plan §0.5).
	workItemHit bool
}

func (s *adoRemediationServerState) start(t *testing.T) *httptest.Server {
	t.Helper()
	prBase := "/" + s.owner + "/" + s.project + "/_apis/git/repositories/" + s.name + "/pullrequests"
	idPath := prBase + "/" + strconv.Itoa(s.prNumber)
	mux := http.NewServeMux()

	mux.HandleFunc(idPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("PR detail method = %s, want GET", r.Method)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		writeJSONResp(t, w, map[string]interface{}{
			"pullRequestId":         s.prNumber,
			"status":                s.status,
			"title":                 "Implement PBI 1456",
			"description":           "Implements PBI 1456\n\nFixes #1456",
			"createdBy":             map[string]string{"displayName": "goober", "uniqueName": "goober@example.com"},
			"isDraft":               false,
			"sourceRefName":         "refs/heads/" + s.prBranch,
			"targetRefName":         "refs/heads/" + s.base,
			"lastMergeSourceCommit": map[string]string{"commitId": s.headSHA},
			"lastMergeTargetCommit": map[string]string{"commitId": s.baseSHA},
			"reviewers":             []interface{}{},
			"repository": map[string]interface{}{
				"id": "repo-guid", "name": s.name,
				"project": map[string]string{"id": "proj-guid", "name": s.project},
			},
		})
	})

	mux.HandleFunc(idPath+"/threads", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("threads method = %s, want GET", r.Method)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		comments := make([]map[string]interface{}, len(s.threadComments))
		for i, body := range s.threadComments {
			comments[i] = map[string]interface{}{
				"id":            i + 1,
				"content":       body,
				"commentType":   "text",
				"author":        map[string]string{"displayName": "goobers-bot", "uniqueName": "bot@example.com"},
				"publishedDate": "2026-07-15T00:00:00Z",
			}
		}
		writeJSONResp(t, w, map[string]interface{}{
			"value": []map[string]interface{}{{
				"id": 10, "status": "active", "comments": comments,
			}},
		})
	})

	// GET the labels (with ids) — RemovePullRequestLabel resolves the id here
	// before deleting, because ADO 400s on delete-by-name for colon-bearing
	// names. The id embeds the name so the DELETE can map it back.
	mux.HandleFunc(idPath+"/labels", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("labels method = %s, want GET", r.Method)
		}
		s.mu.Lock()
		vals := make([]map[string]interface{}, 0, len(s.labels))
		for _, l := range s.labels {
			vals = append(vals, map[string]interface{}{"id": "labelid-" + l, "name": l})
		}
		s.mu.Unlock()
		writeJSONResp(t, w, map[string]interface{}{"value": vals})
	})
	mux.HandleFunc(idPath+"/labels/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("label method = %s, want DELETE", r.Method)
		}
		// Delete-by-id: map the id back to the label name for the assertions.
		name := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, idPath+"/labels/"), "labelid-")
		s.mu.Lock()
		var kept []string
		for _, l := range s.labels {
			if l != name {
				kept = append(kept, l)
			}
		}
		s.labels = kept
		s.deletedLabels = append(s.deletedLabels, name)
		s.mu.Unlock()
		writeJSONResp(t, w, map[string]interface{}{})
	})

	mux.HandleFunc("/"+s.owner+"/"+s.project+"/_apis/policy/evaluations", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]interface{}{"value": []interface{}{}})
	})

	// Any wit/workitems traffic is the PR-as-work-item wrong-object hazard.
	mux.HandleFunc("/"+s.owner+"/"+s.project+"/_apis/wit/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.workItemHit = true
		s.mu.Unlock()
		t.Errorf("unexpected wit/workitems call %s %s — the PR-as-work-item wrong-object hazard", r.Method, r.URL.Path)
		http.Error(w, "work-item mutation is the wrong-object hazard", http.StatusNotImplemented)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// installADOStageProvider points the per-stage ADO provider at server and sets
// the routed-repo env so providerRepo returns the ADO repo. Mirrors the
// merge-review ADO stage tests.
func installADOStageProvider(t *testing.T, repo providers.RepositoryRef, server *httptest.Server) {
	t.Helper()
	original := newADOProviderForStage
	newADOProviderForStage = func(_ string, routed providers.RepositoryRef) (*providers.ADOProvider, error) {
		return providers.NewADOProvider(routed.Owner, routed.Project, "token",
			func(p *providers.ADOProvider) { p.BaseURL = server.URL }), nil
	}
	t.Cleanup(func() { newADOProviderForStage = original })

	t.Setenv(executor.RepoProviderEnvVar, string(repo.Provider))
	t.Setenv(executor.RepoOwnerEnvVar, repo.Owner)
	t.Setenv(executor.RepoProjectEnvVar, repo.Project)
	t.Setenv(executor.RepoNameEnvVar, repo.Name)
}

// pushRemediatedADOFixture is the ADO analog of pushRemediatedFixture: a bare
// origin with the PR branch pushed, a worktree checked out on that branch
// carrying one further LOCAL commit (what `implement` committed and deliberately
// did not push), an ADO REST fake serving that PR + its threads + its labels, and
// a claim-ledger entry for this run. recordHeadSHA seeds the pre-remediation head
// SHA on a PR *thread* (not an issue comment) — the lease expectation
// push-remediated reads back via ListPullRequestThreadComments.
func pushRemediatedADOFixture(t *testing.T, recordHeadSHA bool) (root string, st *adoRemediationServerState, wtPath, remoteTip string) {
	t.Helper()
	root, repo := providerDispatchFixture(t, providers.ProviderADO)
	origin, headSHA, baseSHA := initPRBranchOrigin(t, remediationPRBranch)

	mgr, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	wt, err := mgr.Create(t.Context(), worktree.CreateOptions{
		RepoURL: origin, RunID: "run-392-ado", BaseRef: "main",
		Branch: "goobers/pr-remediation/run-392-ado",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = wt.Remove(t.Context(), worktree.RemoveOptions{}) })
	if _, err := checkoutExistingBranch(wt.Path, remediationPRBranch, "test-token"); err != nil {
		t.Fatalf("checkoutExistingBranch: %v", err)
	}
	// The rework `implement` committed but never pushed.
	if err := os.WriteFile(filepath.Join(wt.Path, "remediated.txt"), []byte("addressed the finding\n"), 0o644); err != nil {
		t.Fatalf("write rework: %v", err)
	}
	runGitT(t, wt.Path, "add", "-A")
	runGitT(t, wt.Path, "commit", "-m", "address the merge-review findings")

	st = &adoRemediationServerState{
		owner: repo.Owner, project: repo.Project, name: repo.Name,
		prNumber: 77, prBranch: remediationPRBranch, base: "main",
		headSHA: headSHA, baseSHA: baseSHA, status: "active",
		labels: []string{needsRemediationLabel},
	}
	if recordHeadSHA {
		comment, err := remediationStateComment(remediationState{
			Cycles: 1, LastDiffDigest: "sha256:prior", HeadSHA: headSHA, BaseSHA: baseSHA,
		})
		if err != nil {
			t.Fatalf("remediationStateComment: %v", err)
		}
		st.threadComments = []string{comment}
	}
	server := st.start(t)
	installADOStageProvider(t, repo, server)

	t.Setenv("GOOBERS_RUN_ID", "run-392-ado")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_REPO_PUSH", "test-token")
	t.Chdir(wt.Path)

	if _, err := claimPullRequestInOrder(root, []providers.PullRequestSummary{{Number: 77}}, "run-392-ado", "pr-remediation", time.Hour); err != nil {
		t.Fatalf("seed PR claim: %v", err)
	}
	return root, st, wt.Path, headSHA
}

// TestPushRemediatedADOPublishesAndClearsLabel is the ADO terminal acceptance for
// push-remediated: the agentic chain's committed rework actually reaches the PR
// branch via force-with-lease, and goobers:needs-remediation is cleared through
// the native PR-label DELETE — never UpdateWorkItem(ID: PR#) — so merge-review
// re-evaluates the reworked PR. The lease expectation is recovered from the PR
// thread's sticky remediation-state comment.
func TestPushRemediatedADOPublishesAndClearsLabel(t *testing.T) {
	root, st, wtPath, remoteTip := pushRemediatedADOFixture(t, true)

	code, stdout, stderr := runArgs(t, "push-remediated", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "#77") {
		t.Errorf("stdout = %q, want a mention of PR #77", stdout)
	}
	pushResult := readCheckpointResult(t, filepath.Join(wtPath, pushRemediatedResultName))
	if pushResult["published"] != "true" || pushResult["selectedNumber"] != "77" {
		t.Errorf("push result = %v, want published=true for PR 77", pushResult)
	}

	// The remote branch really moved to the local rework.
	local := strings.TrimSpace(runGitOutputT(t, wtPath, "rev-parse", "HEAD"))
	pushed := strings.TrimSpace(runGitOutputT(t, wtPath, "ls-remote", "origin", "refs/heads/"+remediationPRBranch))
	pushedSHA, _, _ := strings.Cut(pushed, "\t")
	if pushedSHA != local {
		t.Errorf("remote %s = %q, want the locally reworked tip %q", remediationPRBranch, pushedSHA, local)
	}
	if pushedSHA == remoteTip {
		t.Error("remote branch did not move; the rework was never published")
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	for _, l := range st.labels {
		if l == needsRemediationLabel {
			t.Errorf("labels = %v, want %s cleared so merge-review re-evaluates the PR", st.labels, needsRemediationLabel)
		}
	}
	if len(st.deletedLabels) != 1 || st.deletedLabels[0] != needsRemediationLabel {
		t.Errorf("deletedLabels = %v, want exactly [%s] cleared via the native PR-label DELETE", st.deletedLabels, needsRemediationLabel)
	}
	if st.workItemHit {
		t.Error("a wit/workitems endpoint was called — the PR-as-work-item wrong-object hazard was hit")
	}
}

// TestPushRemediatedADORefusesWithoutThreadRecordedHeadSHA proves the lease
// expectation genuinely comes from the PR *thread* on ADO: with no sticky
// remediation-state comment on the thread, the stage refuses to force-push
// (rather than falling back to a bare push) and leaves the label set.
func TestPushRemediatedADORefusesWithoutThreadRecordedHeadSHA(t *testing.T) {
	root, st, _, _ := pushRemediatedADOFixture(t, false)

	code, _, stderr := runArgs(t, "push-remediated", root)
	if code != 1 {
		t.Fatalf("code = %d, stderr = %q, want 1 (refuse without a recorded lease SHA)", code, stderr)
	}
	if !strings.Contains(stderr, "no recorded pre-remediation head SHA") {
		t.Fatalf("stderr = %q, want the missing-lease-SHA refusal", stderr)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.deletedLabels) != 0 {
		t.Fatalf("deletedLabels = %v, want the label untouched when the push is refused", st.deletedLabels)
	}
	found := false
	for _, l := range st.labels {
		if l == needsRemediationLabel {
			found = true
		}
	}
	if !found {
		t.Fatalf("labels = %v, want %s left set for the next cycle", st.labels, needsRemediationLabel)
	}
}
