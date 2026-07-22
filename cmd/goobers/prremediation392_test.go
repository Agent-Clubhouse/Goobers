package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// The head ref newRemediationCheckpointServer hardcodes; reused here so the
// fake PR's branch and the real git branch under test are the same thing.
const remediationPRBranch = "goobers/impl/remediation-364"

// readCheckpointResult loads the result file remediation-checkpoint writes
// when a resultFile input is declared — the routing contract checkpoint-gate
// and push-remediated both depend on.
func readCheckpointResult(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read checkpoint result: %v", err)
	}
	var out map[string]string
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal checkpoint result %q: %v", raw, err)
	}
	return out
}

// TestGatherPRContextEmitsWorkspaceBranch is the producer half of #392's
// runner contract: gather-pr-context must publish the selected PR's head
// branch under the well-known workspaceBranch key, because that is what moves
// every later stage's worktree onto the PR's branch. The value is asserted
// equal to head — a rebinding to anything else would remediate a different
// tree than the one that was selected.
func TestGatherPRContextEmitsWorkspaceBranch(t *testing.T) {
	const prBranch = "goobers/impl/run-392"
	origin, headSHA, baseSHA := initPRBranchOrigin(t, prBranch)

	srv := gatherPRContextServer{
		owner: "your-org", repo: "your-repo",
		prNumber: 55, head: prBranch, base: "main",
		headSHA: headSHA, baseSHA: baseSHA,
		labels: []string{needsRemediationLabel},
	}
	server := srv.start(t)
	prev := newGitHubProvider
	newGitHubProvider = mergePRTestServer{url: server.URL}.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = prev })

	mgr, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	wt, err := mgr.Create(t.Context(), worktree.CreateOptions{
		RepoURL: origin, RunID: "run-392", BaseRef: "main",
		Branch: "goobers/pr-remediation/run-392",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = wt.Remove(t.Context(), worktree.RemoveOptions{}) })

	instanceRoot := initDemo(t)
	t.Setenv("GOOBERS_RUN_ID", "run-392")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_REPO_PUSH", "test-token")
	t.Chdir(wt.Path)

	code, stdout, stderr := runArgs(t, "gather-pr-context", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	raw, err := os.ReadFile(filepath.Join(wt.Path, remediationBriefResultFile))
	if err != nil {
		t.Fatalf("read %s: %v", remediationBriefResultFile, err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal %s: %v (data=%s)", remediationBriefResultFile, err, raw)
	}

	branch, ok := result["workspaceBranch"].(string)
	if !ok || branch == "" {
		t.Fatalf("result = %v, want a non-empty workspaceBranch", result)
	}
	head, _ := result["head"].(string)
	if branch != head {
		t.Errorf("workspaceBranch = %q, head = %q; the rebinding must name the PR's own branch", branch, head)
	}
}

// TestRemediationCheckpointSignalsContinueOnHealthyCycle covers the routing
// output checkpoint-gate branches on. A healthy cycle must say "true" — this
// is the branch that reaches the agentic chain at all, and the whole of #392.
func TestRemediationCheckpointSignalsContinueOnHealthyCycle(t *testing.T) {
	baseSHA, headSHA := initRemediationCheckpointRepo(t, remediationPRBranch)
	st := &remediationCheckpointServerState{number: 77, headSHA: headSHA, baseSHA: baseSHA, labels: []string{needsRemediationLabel}}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)

	instanceRoot := remediationCheckpointEnv(t, server.URL, false)
	resultFile := filepath.Join(t.TempDir(), "checkpoint-result.json")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", resultFile)

	code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	got := readCheckpointResult(t, resultFile)
	if got["continueRemediation"] != "true" {
		t.Errorf("continueRemediation = %q, want \"true\" on a healthy cycle", got["continueRemediation"])
	}
	// Echoed forward because a gate never updates the InputsFrom chain and
	// push-remediated sits past the agentic chain.
	if got["selectedNumber"] != "77" {
		t.Errorf("selectedNumber = %q, want \"77\"", got["selectedNumber"])
	}
	if got["head"] != remediationPRBranch {
		t.Errorf("head = %q, want %q", got["head"], remediationPRBranch)
	}
	if got["headSha"] != headSHA {
		t.Errorf("headSha = %q, want the pre-remediation remote tip %q (push-remediated's lease expectation)", got["headSha"], headSHA)
	}
}

// TestRemediationCheckpointSignalsHaltOnEscalation is the other side: an
// escalated cycle must NOT reach the agentic chain. Spending a session on a
// PR that just got parked for a human is exactly the waste D4/D5 exist to
// prevent.
func TestRemediationCheckpointSignalsHaltOnEscalation(t *testing.T) {
	baseSHA, headSHA := initRemediationCheckpointRepo(t, remediationPRBranch)
	prior, err := remediationStateComment(remediationState{Cycles: 1, LastDiffDigest: "sha256:unrelated"})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	st := &remediationCheckpointServerState{
		number: 77, headSHA: headSHA, baseSHA: baseSHA,
		labels: []string{needsRemediationLabel}, comments: []string{prior},
	}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)

	instanceRoot := remediationCheckpointEnv(t, server.URL, false)
	resultFile := filepath.Join(t.TempDir(), "checkpoint-result.json")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", resultFile)

	code, stdout, stderr := runArgs(t, "remediation-checkpoint", "--budget", "1", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if got := readCheckpointResult(t, resultFile)["continueRemediation"]; got != "false" {
		t.Errorf("continueRemediation = %q, want \"false\" on an escalated cycle", got)
	}
}

// TestRemediationCheckpointForcedEscalate covers the reviewer-verdict=fail
// path (design doc §4 D2). A terminal "fail" verdict must park the PR
// immediately with the reviewer's reason, rather than let it cycle up to the
// liberal budget re-attempting an approach already judged wrong — and the
// caller's reason must win the prose over the generic budget text.
func TestRemediationCheckpointForcedEscalate(t *testing.T) {
	baseSHA, headSHA := initRemediationCheckpointRepo(t, remediationPRBranch)
	st := &remediationCheckpointServerState{number: 77, headSHA: headSHA, baseSHA: baseSHA, labels: []string{needsRemediationLabel}}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)

	instanceRoot := remediationCheckpointEnv(t, server.URL, false)
	const reason = "the reviewer returned a terminal fail verdict"
	code, stdout, stderr := runArgs(t, "remediation-checkpoint", "--escalate", reason, instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "escalated") {
		t.Errorf("stdout = %q, want an escalation", stdout)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	escalated := false
	for _, l := range st.labels {
		if l == remediationEscalatedLabel {
			escalated = true
		}
		if l == needsRemediationLabel {
			t.Errorf("labels = %v, want needs-remediation cleared on escalation", st.labels)
		}
	}
	if !escalated {
		t.Errorf("labels = %v, want %s added", st.labels, remediationEscalatedLabel)
	}
	if len(st.comments) != 1 {
		t.Fatalf("comments = %v, want exactly one recorded", st.comments)
	}
	state, ok := parseRemediationStateComment(st.comments[0])
	if !ok || !state.Escalated {
		t.Fatalf("comment %q -> state=%+v ok=%v, want an escalated state", st.comments[0], state, ok)
	}
	if state.EscalatedReason != reason {
		t.Errorf("EscalatedReason = %q, want the caller's reason %q — a forced escalation must not be reported as budget exhaustion", state.EscalatedReason, reason)
	}
}

// TestRemediationCheckpointRecoversPRFromClaimLedger covers the fallback the
// --escalate path needs: running past the agentic chain, Task.InputsFrom can
// no longer reach gather-pr-context's selectedNumber, so the run's own
// durable PR claim is the source of truth.
func TestRemediationCheckpointRecoversPRFromClaimLedger(t *testing.T) {
	baseSHA, headSHA := initRemediationCheckpointRepo(t, remediationPRBranch)
	st := &remediationCheckpointServerState{number: 77, headSHA: headSHA, baseSHA: baseSHA, labels: []string{needsRemediationLabel}}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)

	instanceRoot := remediationCheckpointEnv(t, server.URL, false)
	// Deliberately clear the threaded input: this is the post-agentic-chain
	// situation, where nothing can thread it.
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "")
	if _, err := claimPullRequest(instanceRoot, []providers.PullRequestSummary{{Number: 77}}, "run-364", "pr-remediation", time.Hour); err != nil {
		t.Fatalf("seed PR claim: %v", err)
	}

	code, stdout, stderr := runArgs(t, "remediation-checkpoint", "--escalate", "reviewer said fail", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "#77") {
		t.Errorf("stdout = %q, want the PR recovered from the claim ledger", stdout)
	}
}

// pushRemediatedFixture stands the whole tail of a remediation cycle up: a
// bare origin with the PR branch pushed, a worktree checked out on that branch
// carrying one further LOCAL commit (what `implement` would have committed and
// deliberately not pushed), a fake GitHub serving that PR, and a claim ledger
// entry for this run. Returns the instance root, the fake server's state, the
// worktree path, and the PR branch's remote tip before anything is pushed —
// the force-with-lease expectation.
func pushRemediatedFixture(t *testing.T, recordHeadSHA bool) (instanceRoot string, st *remediationCheckpointServerState, wtPath, remoteTip string) {
	t.Helper()
	origin, headSHA, baseSHA := initPRBranchOrigin(t, remediationPRBranch)

	mgr, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	wt, err := mgr.Create(t.Context(), worktree.CreateOptions{
		RepoURL: origin, RunID: "run-392-push", BaseRef: "main",
		Branch: "goobers/pr-remediation/run-392-push",
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

	st = &remediationCheckpointServerState{
		number: 77, headSHA: headSHA, baseSHA: baseSHA,
		labels: []string{needsRemediationLabel},
	}
	if recordHeadSHA {
		// What remediation-checkpoint recorded earlier in this same run.
		comment, err := remediationStateComment(remediationState{
			Cycles: 1, LastDiffDigest: "sha256:prior", HeadSHA: headSHA, BaseSHA: baseSHA,
		})
		if err != nil {
			t.Fatalf("remediationStateComment: %v", err)
		}
		st.comments = []string{comment}
	}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)

	instanceRoot = initDemo(t)
	prev := newGitHubProvider
	newGitHubProvider = mergePRTestServer{url: server.URL}.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = prev })

	t.Setenv("GOOBERS_RUN_ID", "run-392-push")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_REPO_PUSH", "test-token")
	t.Chdir(wt.Path)

	if _, err := claimPullRequest(instanceRoot, []providers.PullRequestSummary{{Number: 77}}, "run-392-push", "pr-remediation", time.Hour); err != nil {
		t.Fatalf("seed PR claim: %v", err)
	}
	return instanceRoot, st, wt.Path, headSHA
}

// TestPushRemediatedPublishesAndClearsLabel is #392's terminal acceptance: the
// agentic chain's committed rework actually reaches the PR, and the label is
// cleared so merge-review re-evaluates it. Without this the run would report
// success having changed nothing — the work would die with the worktree.
func TestPushRemediatedPublishesAndClearsLabel(t *testing.T) {
	instanceRoot, st, wtPath, remoteTip := pushRemediatedFixture(t, true)

	code, stdout, stderr := runArgs(t, "push-remediated", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "#77") {
		t.Errorf("stdout = %q, want a mention of PR #77", stdout)
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
}

// TestPushRemediatedRefusesWithoutRecordedLeaseSHA is the safety property. A
// bare force-push would clobber a human who pushed to the branch mid-cycle,
// which is precisely what design doc §5 says must never happen ("the lease
// makes Goobers lose gracefully and re-select next tick"). With no recorded
// pre-remediation SHA there is no honest lease to take, so the stage must fail
// closed rather than push anyway.
func TestPushRemediatedRefusesWithoutRecordedLeaseSHA(t *testing.T) {
	instanceRoot, _, wtPath, remoteTip := pushRemediatedFixture(t, false)

	code, stdout, stderr := runArgs(t, "push-remediated", instanceRoot)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (business error); stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "lease") {
		t.Errorf("stderr = %q, want it to name the missing lease expectation", stderr)
	}
	pushed := strings.TrimSpace(runGitOutputT(t, wtPath, "ls-remote", "origin", "refs/heads/"+remediationPRBranch))
	pushedSHA, _, _ := strings.Cut(pushed, "\t")
	if pushedSHA != remoteTip {
		t.Errorf("remote %s = %q, want it untouched at %q — refusing must not push", remediationPRBranch, pushedSHA, remoteTip)
	}
}

// TestPushRemediatedLosesGracefullyToAConcurrentPush is the lease doing its
// actual job end-to-end: a human pushes to the PR branch while the agentic
// chain is running, and the stage must refuse rather than discard their work.
func TestPushRemediatedLosesGracefullyToAConcurrentPush(t *testing.T) {
	instanceRoot, _, wtPath, _ := pushRemediatedFixture(t, true)

	// A human pushes to the same branch after the cycle captured its lease.
	origin := strings.TrimSpace(runGitOutputT(t, wtPath, "remote", "get-url", "origin"))
	humanRoot := t.TempDir()
	human := filepath.Join(humanRoot, "human")
	runGitT(t, humanRoot, "clone", "--branch", remediationPRBranch, origin, human)
	runGitT(t, human, "config", "user.name", "human")
	runGitT(t, human, "config", "user.email", "human@example.com")
	if err := os.WriteFile(filepath.Join(human, "human.txt"), []byte("a human was here\n"), 0o644); err != nil {
		t.Fatalf("write human change: %v", err)
	}
	runGitT(t, human, "add", "-A")
	runGitT(t, human, "commit", "-m", "human push mid-remediation")
	runGitT(t, human, "push", "origin", remediationPRBranch)
	humanSHA := strings.TrimSpace(runGitOutputT(t, human, "rev-parse", "HEAD"))

	code, stdout, stderr := runArgs(t, "push-remediated", instanceRoot)
	if code != 1 {
		t.Fatalf("code = %d, want 1 — the lease must refuse; stdout = %q, stderr = %q", code, stdout, stderr)
	}
	pushed := strings.TrimSpace(runGitOutputT(t, wtPath, "ls-remote", "origin", "refs/heads/"+remediationPRBranch))
	pushedSHA, _, _ := strings.Cut(pushed, "\t")
	if pushedSHA != humanSHA {
		t.Errorf("remote %s = %q, want the human's commit %q preserved", remediationPRBranch, pushedSHA, humanSHA)
	}
}

// TestClaimedPullRequestNumberIgnoresOtherRuns pins the recovery helper's
// scoping: a stage must never pick up a PR another run holds. Getting this
// wrong would force-push one run's rework onto a different run's PR.
func TestClaimedPullRequestNumberIgnoresOtherRuns(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_RUN_ID", "run-mine")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")

	if _, err := claimPullRequest(root, []providers.PullRequestSummary{{Number: 91}}, "run-theirs", "pr-remediation", time.Hour); err != nil {
		t.Fatalf("seed other run's claim: %v", err)
	}
	if _, ok, err := claimedPullRequestNumber(root); err != nil || ok {
		t.Fatalf("claimedPullRequestNumber = ok %v, err %v; want no claim for this run", ok, err)
	}

	if _, err := claimPullRequest(root, []providers.PullRequestSummary{{Number: 77}}, "run-mine", "pr-remediation", time.Hour); err != nil {
		t.Fatalf("seed own claim: %v", err)
	}
	number, ok, err := claimedPullRequestNumber(root)
	if err != nil || !ok {
		t.Fatalf("claimedPullRequestNumber = ok %v, err %v; want this run's own claim", ok, err)
	}
	if number != 77 {
		t.Errorf("number = %d, want 77", number)
	}
}

// TestRemediationCheckpointPreservesAnUnpushedRebase is the regression test for
// the defect #392's own topology introduced.
//
// On the substantive-findings path rebase-pr rebases but deliberately does NOT
// push (rebasepr.go's `!conflict && !hasSubstantiveFindings` guard), so the
// rebase exists only as a local commit. remediation-checkpoint then runs
// between rebase-pr and implement, and its re-checkout is `git checkout -B
// <branch> FETCH_HEAD` — a hard reset to the REMOTE tip. Left unguarded that
// silently discarded the rebase, handed implement an un-rebased tree, and left
// the PR still behind base after a "successful" remediation, looping it until
// the D4 budget escalated.
func TestRemediationCheckpointPreservesAnUnpushedRebase(t *testing.T) {
	baseSHA, headSHA := initRemediationCheckpointRepo(t, remediationPRBranch)

	// Stand in for the worktree rebase-pr left behind: on the PR's branch,
	// carrying a local commit that is NOT on the remote (as a local rebase is).
	runGitT(t, ".", "fetch", "origin", "refs/heads/"+remediationPRBranch)
	runGitT(t, ".", "checkout", "-B", remediationPRBranch, "FETCH_HEAD")
	runGitT(t, ".", "config", "user.name", "rebase")
	runGitT(t, ".", "config", "user.email", "rebase@example.com")
	if err := os.WriteFile("rebased.txt", []byte("carried by the local rebase\n"), 0o644); err != nil {
		t.Fatalf("write rebase marker: %v", err)
	}
	runGitT(t, ".", "add", "-A")
	runGitT(t, ".", "commit", "-m", "local rebase, not pushed")
	localTip := strings.TrimSpace(runGitOutputT(t, ".", "rev-parse", "HEAD"))
	if localTip == headSHA {
		t.Fatal("fixture did not actually advance the local branch")
	}

	st := &remediationCheckpointServerState{number: 77, headSHA: headSHA, baseSHA: baseSHA, labels: []string{needsRemediationLabel}}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
	instanceRoot := remediationCheckpointEnv(t, server.URL, false)

	code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	after := strings.TrimSpace(runGitOutputT(t, ".", "rev-parse", "HEAD"))
	if after != localTip {
		t.Fatalf("HEAD = %q after checkpoint, want the unpushed rebase %q preserved (reset to remote %q loses it)", after, localTip, headSHA)
	}
	if _, err := os.Stat("rebased.txt"); err != nil {
		t.Errorf("the rebase's file is gone after checkpoint: %v", err)
	}
}

// TestPushRemediatedRefusesToPublishAnUnchangedBranch covers the case where the
// agentic chain produced nothing — an implement session that timed out or
// no-op'd, then a reviewer that passed the PR's PRE-EXISTING diff (on a
// re-entered branch that diff is never empty, so #415's empty-diff fast-fail
// cannot fire). Pushing would be harmless, but clearing needs-remediation would
// hand merge-review a PR it already rejected, unchanged, as if remediated.
func TestPushRemediatedRefusesToPublishAnUnchangedBranch(t *testing.T) {
	origin, headSHA, baseSHA := initPRBranchOrigin(t, remediationPRBranch)

	mgr, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	wt, err := mgr.Create(t.Context(), worktree.CreateOptions{
		RepoURL: origin, RunID: "run-392-noop", BaseRef: "main",
		Branch: "goobers/pr-remediation/run-392-noop",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = wt.Remove(t.Context(), worktree.RemoveOptions{}) })
	// On the PR's branch, but with NO new commit — the no-op remediation.
	if _, err := checkoutExistingBranch(wt.Path, remediationPRBranch, "test-token"); err != nil {
		t.Fatalf("checkoutExistingBranch: %v", err)
	}

	comment, err := remediationStateComment(remediationState{
		Cycles: 1, LastDiffDigest: "sha256:prior", HeadSHA: headSHA, BaseSHA: baseSHA,
	})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	st := &remediationCheckpointServerState{
		number: 77, headSHA: headSHA, baseSHA: baseSHA,
		labels: []string{needsRemediationLabel}, comments: []string{comment},
	}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)

	instanceRoot := initDemo(t)
	prev := newGitHubProvider
	newGitHubProvider = mergePRTestServer{url: server.URL}.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = prev })
	t.Setenv("GOOBERS_RUN_ID", "run-392-noop")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_REPO_PUSH", "test-token")
	t.Chdir(wt.Path)
	if _, err := claimPullRequest(instanceRoot, []providers.PullRequestSummary{{Number: 77}}, "run-392-noop", "pr-remediation", time.Hour); err != nil {
		t.Fatalf("seed PR claim: %v", err)
	}

	code, stdout, stderr := runArgs(t, "push-remediated", instanceRoot)
	if code != 1 {
		t.Fatalf("code = %d, want 1 for a no-op remediation; stdout = %q, stderr = %q", code, stdout, stderr)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	found := false
	for _, l := range st.labels {
		if l == needsRemediationLabel {
			found = true
		}
	}
	if !found {
		t.Errorf("labels = %v, want %s STILL set — an unremediated PR must not look remediated", st.labels, needsRemediationLabel)
	}
}

// TestPushRemediatedFailsWhenItsClaimLeaseExpired pins the honest reporting of
// the one way this stage can lose its PR. pr-remediation never releases the
// claim mid-run, so an absent entry means `goobers up`'s RecoverExpired reaped
// an expired lease — the rework is committed but unpublished. Reporting success
// there (issue-close-out's no-op contract, which does not apply here) would
// claim the work shipped when it was silently dropped.
func TestPushRemediatedFailsWhenItsClaimLeaseExpired(t *testing.T) {
	instanceRoot, st, _, _ := pushRemediatedFixture(t, true)
	if err := releaseClaimsForRun(layoutFor(instanceRoot), nil, "run-392-push"); err != nil {
		t.Fatalf("release claim: %v", err)
	}

	code, stdout, stderr := runArgs(t, "push-remediated", instanceRoot)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "lease expired") {
		t.Errorf("stderr = %q, want it to name the expired lease", stderr)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, l := range st.labels {
		if l == needsRemediationLabel {
			return
		}
	}
	t.Errorf("labels = %v, want %s still set so the PR is re-selected", st.labels, needsRemediationLabel)
}
