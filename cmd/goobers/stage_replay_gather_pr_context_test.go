package main

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// replayGatherPRContextGitHub replays gather-pr-context's unchanged-digest
// park. The PR carries two escalation records for the same diff digest: the
// implementation-escalation payload open-pr published in its body, and the
// sticky remediation-state comment an earlier escalation left, stored with
// its attribution footer. One unchanged-digest tick is already on record, so
// this run is the one that parks.
//
// The park must find the sticky comment among the PR's comments and edit it
// by id. Reading it back through the footer is what keeps the park to one
// comment: if the comment were not recognised, the stage would fall back to
// the body's record, which names no comment, and post a second one. Run 2 is
// the same run retried: the PR is parked now, and the stage reads its own
// comment back again to keep it excluded.
func replayGatherPRContextGitHub(t *testing.T) replayFixture {
	const (
		runID    = "replay-gather-pr-context"
		prNumber = 77
		prBranch = "goobers/implementation/replay-77"
	)
	origin, headSHA, baseSHA := initPRBranchOrigin(t, prBranch)
	mgr, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	wt, err := mgr.Create(t.Context(), worktree.CreateOptions{
		RepoURL: origin, RunID: runID, BaseRef: "main",
		Branch: "goobers/pr-remediation/" + runID,
	})
	if err != nil {
		t.Fatalf("create stage worktree: %v", err)
	}
	t.Cleanup(func() { _ = wt.Remove(t.Context(), worktree.RemoveOptions{}) })
	if _, err := checkoutExistingBranchWithAuth(t.Context(), wt.Path, prBranch, tokenGitAuthEnvironment("")); err != nil {
		t.Fatalf("checkout PR branch: %v", err)
	}
	digest, err := diffDigest(wt.Path, baseSHA)
	if err != nil {
		t.Fatalf("diffDigest: %v", err)
	}
	body, err := implementationEscalationMarker(implementationEscalationState{
		DiffDigest: digest, Reason: "the implementer produced the same diff twice",
	})
	if err != nil {
		t.Fatalf("implementation escalation marker: %v", err)
	}
	sticky := renderRemediationComment(remediationState{
		Cycles: 2, LastDiffDigest: digest, HeadSHA: headSHA, BaseSHA: baseSHA,
		Escalated: true, EscalatedReason: "remediation produced a byte-identical diff",
		EscalationOutcome: remediationOutcomeDidNotConverge,
		EscalatedHeadSHA:  headSHA, EscalatedBaseSHA: baseSHA, EscalationGeneration: 1,
	})

	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(prNumber, "Replay PR", needsRemediationLabel)
	server.addOpenPR(prNumber, prBranch, "main", headSHA, baseSHA, false, []string{needsRemediationLabel}, nil)
	server.setPRBody(prNumber, body)
	server.setBranchTip("main", baseSHA)
	server.addCommentAs(prNumber, "reviewer", "Still the same diff as last time.")
	server.addComment(prNumber, sticky)

	counter := &providerWriteCounter{}
	recorded := recordServerWrites(t, replayPRLabelsFollowIssue(server, prNumber), counter)
	previous := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		return providers.NewGitHubProvider(token, append(opts, func(p *providers.GitHubProvider) { p.BaseURL = recorded.URL })...)
	}
	t.Cleanup(func() { newGitHubProvider = previous })

	root := initDemo(t)
	seedRemediationNoopState(t, layoutFor(root), remediationNoopKey("", prNumber),
		remediationNoopSignature{HeadSHA: headSHA, DiffDigest: digest}, "prior-unchanged-digest-run")
	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_REPO_PUSH", "test-token")
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderGitHub))
	t.Setenv(executor.RepoOwnerEnvVar, "your-org")
	t.Setenv(executor.RepoNameEnvVar, "your-repo")
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), filepath.Join(wt.Path, remediationBriefResultFile))
	t.Chdir(wt.Path)

	runs := 0
	return replayFixture{
		run: func(t *testing.T) (int, string, string) {
			runs++
			code, stdout, stderr := runArgs(t, "gather-pr-context", root)
			if code == 0 && runs == 1 && !strings.Contains(stdout, "visibly parked") {
				return 1, stdout, "run 1 did not park the unchanged-digest PR: " + stderr
			}
			return code, stdout, stderr
		},
		writes: counter,
		owned: func(t *testing.T) map[string][]string {
			return map[string][]string{"remediation state": ownedGitHubComments(t, server, prNumber, func(body string) bool {
				_, ok := parseRemediationStateComment(body)
				return ok
			})}
		},
	}
}

// replayPRLabelsFollowIssue serves the fake GitHub server with number's pull
// request labels kept equal to its issue labels, as GitHub stores them: the
// stage writes labels through the issues API and lists them on the pull.
func replayPRLabelsFollowIssue(server *fakeGitHubServer, number int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server.server.Config.Handler.ServeHTTP(w, r)
		server.mu.Lock()
		defer server.mu.Unlock()
		server.prs[number].labels = append([]string(nil), server.issues[number].labels...)
	})
}
