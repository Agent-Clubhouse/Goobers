package main

import (
	"net/http"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// update-behind-pr's replay case: the merge-review / pr-remediation round
// trip through the attributed merge-review status comment.
//
// update-behind-pr writes no text of its own. What it reads back is the
// sticky merge-review status comment apply-verdict posted under Goobers' own
// login, which a daemon run stores with the attribution footer. It must still
// recognise that comment as the canonical status and recover its verdict: a
// substantive finding routes the pull request to full remediation, and a
// comment the stage fails to recognise instead reads as "no verdict", so the
// stage pushes an API branch update and clears goobers:needs-remediation on a
// pull request that still carries a defect.
//
// The verdict key-value record would short-circuit the comment read, so
// apply-verdict runs under its own instance root, as a verdict recorded
// before #5030 or by another instance is: update-behind-pr then has only the
// comment to go on, and migrates it on read. Both update-behind-pr runs must
// route to full remediation and write nothing.

const (
	replayUpdateBehindPR       = 55
	replayUpdateBehindHead     = "sha55head"
	replayUpdateBehindOpenBase = "shamainbase"
	replayUpdateBehindLiveBase = "shamainlive"
)

func replayUpdateBehindPRGitHub(t *testing.T) replayFixture {
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(replayUpdateBehindPR, "Behind PR", needsRemediationLabel)
	server.addOpenPR(replayUpdateBehindPR, "goobers/implementation/run-55", "main",
		replayUpdateBehindHead, replayUpdateBehindOpenBase, false, []string{needsRemediationLabel},
		[]fakePRFile{{path: "internal/runner/run.go", status: "modified", additions: 5, deletions: 1}})
	// main moved on after the PR was cut: the PR is behind its live base.
	server.setBranchTip("main", replayUpdateBehindLiveBase)
	server.mu.Lock()
	server.compares[replayUpdateBehindLiveBase+"..."+replayUpdateBehindHead] = fakeCompare{mergeBaseSHA: replayUpdateBehindOpenBase}
	server.mu.Unlock()

	replayMergeReviewStatusVerdict(t, server)

	root := initDemo(t)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-replay-update-behind")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "issues-token")
	for _, name := range []string{"GOOBERS_INPUT_SELECTEDNUMBER", "GOOBERS_INPUT_SELECTEDHEADSHA", "GOOBERS_INPUT_SELECTEDBASESHA"} {
		t.Setenv(name, "")
	}
	counter := &providerWriteCounter{}
	recorded := recordServerWrites(t, replayUpdateBranchRoute(server), counter)
	previous := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		return providers.NewGitHubProvider(token, append(opts, func(p *providers.GitHubProvider) { p.BaseURL = recorded.URL })...)
	}
	t.Cleanup(func() { newGitHubProvider = previous })
	return replayFixture{
		run: func(t *testing.T) (int, string, string) {
			t.Chdir(t.TempDir())
			code, stdout, stderr := runArgs(t, "update-behind-pr", root)
			if code == 0 && !strings.Contains(stdout, "PR #55 requires full remediation") {
				return 1, stdout, "the attributed substantive verdict did not route PR #55 to full remediation: " + stderr
			}
			return code, stdout, stderr
		},
		writes: counter,
		owned: func(t *testing.T) map[string][]string {
			return map[string][]string{"merge-review status": ownedGitHubComments(t, server, replayUpdateBehindPR, isMergeReviewStatusComment)}
		},
	}
}

// replayMergeReviewStatusVerdict runs apply-verdict under its own instance
// root, posting a needs-changes verdict with one substantive finding as the
// attributed merge-review status comment on the pull request.
func replayMergeReviewStatusVerdict(t *testing.T, server *fakeGitHubServer) {
	t.Helper()
	const runID = "run-replay-merge-review"
	mergeReviewRoot := initDemo(t)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", runID)
	t.Setenv("GOOBERS_CRED_GITHUB_PR_REVIEW", "review-token")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "55")
	seedGateVerdictJournal(t, mergeReviewRoot, runID, apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges, Rationale: "the parser drops empty input",
		HeadSHA: replayUpdateBehindHead, BaseSHA: replayUpdateBehindOpenBase,
		Findings: []apiv1.Finding{{Severity: apiv1.SeverityError, Class: apiv1.FindingSubstantive, Message: "validate empty input"}},
	})
	t.Chdir(t.TempDir())
	if code, stdout, stderr := runArgs(t, "apply-verdict", mergeReviewRoot); code != 0 {
		t.Fatalf("apply-verdict: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	status := ownedGitHubComments(t, server, replayUpdateBehindPR, isMergeReviewStatusComment)
	if len(status) != 1 {
		t.Fatalf("apply-verdict left %d merge-review status comments, want one: %q", len(status), status)
	}
	if _, ok, err := providers.ParseAttribution(status[0]); err != nil || !ok {
		t.Fatalf("merge-review status comment is not attributed (ok=%v, err=%v): %q", ok, err, status[0])
	}
}

// replayUpdateBranchRoute adds GitHub's update-branch endpoint, which the
// shared fake does not serve, in front of it. An accepted update merges the
// live base into the head, so the pull request is no longer behind.
func replayUpdateBranchRoute(server *fakeGitHubServer) http.Handler {
	fake := server.server.Config.Handler
	path := "/repos/your-org/your-repo/pulls/55/update-branch"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			fake.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodPut {
			http.Error(w, "want PUT", http.StatusMethodNotAllowed)
			return
		}
		server.mu.Lock()
		server.compares[replayUpdateBehindLiveBase+"..."+replayUpdateBehindHead] = fakeCompare{mergeBaseSHA: replayUpdateBehindLiveBase}
		server.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"message":"Updating pull request branch."}`))
	})
}
