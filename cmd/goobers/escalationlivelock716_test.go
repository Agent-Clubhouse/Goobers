package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// TestEscalationStillBlocks exercises #716's core self-heal decision in
// isolation: a PR not carrying goobers:merge-escalated is never blocked; one
// that carries the label but has no recorded escalation snapshot (e.g.
// escalated before this fix shipped, or hand-labeled) fails closed and stays
// blocked; one whose recorded snapshot still matches its current head/base
// stays blocked (genuinely unchanged, re-selecting would just reproduce the
// same escalation); one whose head OR base SHA has moved since the snapshot
// was recorded is unblocked (AC2's self-heal — new commits, or a sibling
// merge advancing the base via post-merge.go's fan-out).
func TestEscalationStillBlocks(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}

	t.Run("no label, never blocked", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(1, "pr 1")
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{Number: 1, HeadSHA: "h1", BaseSHA: "b1"}

		blocked, err := escalationStillBlocks(context.Background(), provider, repo, pr)
		if err != nil {
			t.Fatalf("escalationStillBlocks: %v", err)
		}
		if blocked {
			t.Fatal("blocked = true, want false — PR carries no merge-escalated label")
		}
	})

	t.Run("labeled but no recorded snapshot fails closed", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(2, "pr 2")
		server.addComment(2, "please rebase, thanks!") // ordinary comment, no payload
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{Number: 2, HeadSHA: "h2", BaseSHA: "b2", Labels: []string{remediationEscalatedLabel}}

		blocked, err := escalationStillBlocks(context.Background(), provider, repo, pr)
		if err != nil {
			t.Fatalf("escalationStillBlocks: %v", err)
		}
		if !blocked {
			t.Fatal("blocked = false, want true — labeled with no snapshot to compare against must fail closed")
		}
	})

	t.Run("unchanged since escalation stays blocked", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(3, "pr 3")
		comment, err := remediationStateComment(remediationState{
			Cycles: 3, LastDiffDigest: "sha256:x", Escalated: true,
			EscalatedReason:  "repass budget exhausted (11/10 cycles)",
			EscalatedHeadSHA: "h3", EscalatedBaseSHA: "b3",
		})
		if err != nil {
			t.Fatalf("remediationStateComment: %v", err)
		}
		server.addComment(3, comment)
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{Number: 3, HeadSHA: "h3", BaseSHA: "b3", Labels: []string{remediationEscalatedLabel}}

		blocked, err := escalationStillBlocks(context.Background(), provider, repo, pr)
		if err != nil {
			t.Fatalf("escalationStillBlocks: %v", err)
		}
		if !blocked {
			t.Fatal("blocked = false, want true — head/base unchanged since the recorded escalation")
		}
	})

	t.Run("new commits self-heal (head changed)", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(4, "pr 4")
		comment, err := remediationStateComment(remediationState{
			Escalated: true, EscalatedHeadSHA: "stale-head", EscalatedBaseSHA: "b4",
		})
		if err != nil {
			t.Fatalf("remediationStateComment: %v", err)
		}
		server.addComment(4, comment)
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{Number: 4, HeadSHA: "fresh-head", BaseSHA: "b4", Labels: []string{remediationEscalatedLabel}}

		blocked, err := escalationStillBlocks(context.Background(), provider, repo, pr)
		if err != nil {
			t.Fatalf("escalationStillBlocks: %v", err)
		}
		if blocked {
			t.Fatal("blocked = true, want false — new commits landed since escalation (head SHA moved)")
		}
	})

	t.Run("sibling merge self-heal (base changed)", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(5, "pr 5")
		comment, err := remediationStateComment(remediationState{
			Escalated: true, EscalatedHeadSHA: "h5", EscalatedBaseSHA: "stale-base",
		})
		if err != nil {
			t.Fatalf("remediationStateComment: %v", err)
		}
		server.addComment(5, comment)
		provider := server.newGitHubProvider("token")
		pr := providers.PullRequestSummary{Number: 5, HeadSHA: "h5", BaseSHA: "advanced-base", Labels: []string{remediationEscalatedLabel}}

		blocked, err := escalationStillBlocks(context.Background(), provider, repo, pr)
		if err != nil {
			t.Fatalf("escalationStillBlocks: %v", err)
		}
		if blocked {
			t.Fatal("blocked = true, want false — a sibling merge advanced the base since escalation (base SHA moved)")
		}
	})
}

// TestPRSelectExcludesUnchangedEscalatedPR is #716 AC1's merge-review-side
// acceptance: an escalated PR whose head/base still match its recorded
// escalation snapshot is not selected.
func TestPRSelectExcludesUnchangedEscalatedPR(t *testing.T) {
	const prNumber = 601
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(prNumber, "stuck pr")
	server.addOpenPR(prNumber, "goobers/implementation/stuck", "main", "headsha-601", "basesha-601", false, []string{remediationEscalatedLabel}, nil)
	comment, err := remediationStateComment(remediationState{
		Escalated: true, EscalatedHeadSHA: "headsha-601", EscalatedBaseSHA: "basesha-601",
		EscalatedReason: "this cycle's diff is byte-identical to the immediately prior cycle's",
	})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	server.addComment(prNumber, comment)

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-run-601")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "no work") {
		t.Fatalf("stdout = %q, want no-work — escalated PR is unchanged since escalation", stdout)
	}
}

// TestPRSelectSelectsSelfHealedEscalatedPR is #716 AC2's merge-review-side
// acceptance: an escalated PR whose head SHA has moved since escalation (new
// commits landed) is selectable again, automatically, with no human
// intervention on the label.
func TestPRSelectSelectsSelfHealedEscalatedPR(t *testing.T) {
	const prNumber = 602
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(prNumber, "self-healed pr")
	server.addOpenPR(prNumber, "goobers/implementation/healed", "main", "new-head-602", "basesha-602", false, []string{remediationEscalatedLabel}, nil)
	comment, err := remediationStateComment(remediationState{
		Escalated: true, EscalatedHeadSHA: "stale-head-602", EscalatedBaseSHA: "basesha-602",
	})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	server.addComment(prNumber, comment)

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-run-602")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "602") {
		t.Fatalf("stdout = %q, want PR #602 selected — new commits self-heal the escalation", stdout)
	}
}

// TestGatherPRContextExcludesEscalatedNeedsRemediationPR is #716's headline
// regression test: it reproduces the ACTUAL incident's root cause. Before
// this fix, gather-pr-context's needsRemediation eligibility branch never
// checked goobers:merge-escalated at all (only the separate failingCI branch
// did) — so a PR carrying BOTH needs-remediation (post-merge.go's fan-out
// blindly re-adds it to every sibling of a just-merged PR, escalated or not)
// AND merge-escalated was still selected via needsRemediation alone,
// re-running remediation and re-escalating forever. This is exactly PR
// #408's ~20-loop/19-hour incident.
func TestGatherPRContextExcludesEscalatedNeedsRemediationPR(t *testing.T) {
	srv := gatherPRContextServer{
		owner: "your-org", repo: "your-repo",
		prNumber: 408, head: "goobers/impl/livelocked", base: "main",
		headSHA: "head-408", baseSHA: "base-408",
		labels: []string{needsRemediationLabel, remediationEscalatedLabel},
	}
	stateComment, err := remediationStateComment(remediationState{
		Escalated: true, EscalatedHeadSHA: "head-408", EscalatedBaseSHA: "base-408",
		EscalatedReason: "this cycle's diff is byte-identical to the immediately prior cycle's",
	})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	srv.comments = []map[string]interface{}{
		{"id": 1, "user": map[string]string{"login": "goobers-bot"}, "body": stateComment, "created_at": "2026-07-16T00:00:00Z"},
	}
	server := srv.start(t)

	prev := newGitHubProvider
	newGitHubProvider = mergePRTestServer{url: server.URL}.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = prev })

	instanceRoot := initDemo(t)
	t.Setenv("GOOBERS_RUN_ID", "run-408-livelock")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_REPO_PUSH", "test-token")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "gather-pr-context", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "no work") {
		t.Fatalf("stdout = %q, want no-work — needs-remediation must not override an unchanged escalation (#716 regression)", stdout)
	}
}

// TestGatherPRContextSelfHealsOnBaseAdvance is #716 AC2's pr-remediation-side
// acceptance: a sibling merge advancing the base (post-merge.go's fan-out,
// which re-labels every open sibling needs-remediation regardless of prior
// escalation state) re-enables selection once the PR's recorded base SHA
// snapshot no longer matches its current one. The escalation snapshot
// comparison uses the fake server's declared head/base SHA fields (what
// escalationStillBlocks actually reads from PullRequestSummary) — independent
// of the real git repo's own SHAs, which checkoutExistingBranch/isBehindBase
// need to exist only so the checkout step itself succeeds.
func TestGatherPRContextSelfHealsOnBaseAdvance(t *testing.T) {
	const prBranch = "goobers/impl/healed-409"
	// isBehindBase actually runs `git merge-base --is-ancestor <baseSHA>
	// HEAD` against the checked-out repo, so the PR's declared baseSHA must
	// be a REAL SHA in that repo — initPRBranchOrigin's own advanced main
	// tip. The escalation snapshot's EscalatedBaseSHA is the one compared
	// purely in-memory (escalationStillBlocks never touches git for it), so
	// an arbitrary stale placeholder there is what represents "recorded
	// before the base advanced."
	origin, headSHA, baseSHA := initPRBranchOrigin(t, prBranch)

	stateComment, err := remediationStateComment(remediationState{
		Escalated: true, EscalatedHeadSHA: headSHA, EscalatedBaseSHA: "stale-base-409",
	})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	srv := gatherPRContextServer{
		owner: "your-org", repo: "your-repo",
		prNumber: 409, head: prBranch, base: "main",
		headSHA: headSHA, baseSHA: baseSHA, // base moved since escalation
		labels: []string{needsRemediationLabel, remediationEscalatedLabel},
		comments: []map[string]interface{}{
			{"id": 1, "user": map[string]string{"login": "goobers-bot"}, "body": stateComment, "created_at": "2026-07-16T00:00:00Z"},
		},
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
		RepoURL: origin, RunID: "run-409-healed", BaseRef: "main",
		Branch: "goobers/pr-remediation/run-409-healed",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = wt.Remove(t.Context(), worktree.RemoveOptions{}) })

	instanceRoot := initDemo(t)
	t.Setenv("GOOBERS_RUN_ID", "run-409-healed")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_REPO_PUSH", "test-token")
	t.Chdir(wt.Path)

	code, stdout, stderr := runArgs(t, "gather-pr-context", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "409") {
		t.Fatalf("stdout = %q, want PR #409 selected — base advance self-heals the escalation", stdout)
	}
}

// TestRemediationCheckpointEscalationCommentIsSticky is #716 AC3's
// acceptance: escalating twice in a row with an unchanged diff (the
// "manually cleared the label but nothing actually changed" scenario AC2's
// digest short-circuit exists for, reached here by re-running the checkpoint
// stage directly) edits the SAME sticky comment rather than growing a new
// one on every run.
func TestRemediationCheckpointEscalationCommentIsSticky(t *testing.T) {
	// newRemediationCheckpointServer's PR fixture hardcodes this branch name.
	baseSHA, headSHA := initRemediationCheckpointRepo(t, "goobers/impl/remediation-364")
	st := &remediationCheckpointServerState{number: 716, headSHA: headSHA, baseSHA: baseSHA, labels: []string{"goobers:needs-remediation"}}
	server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)
	instanceRoot := remediationCheckpointEnv(t, server.URL, false)
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "716")

	// First cycle: records cycle 1 (not escalated).
	if code, _, stderr := runArgs(t, "remediation-checkpoint", instanceRoot); code != 0 {
		t.Fatalf("first cycle: code = %d, stderr = %q", code, stderr)
	}
	// Second cycle: same diff -> escalates, editing the cycle-1 comment.
	code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
	if code != 0 {
		t.Fatalf("second cycle: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "escalated") {
		t.Fatalf("second cycle stdout = %q, want escalation", stdout)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.comments) != 1 {
		t.Fatalf("comments = %v, want exactly 1 — the escalation must edit the sticky comment, not post a new one (#716 AC3)", st.comments)
	}
	state, ok := parseRemediationStateComment(st.comments[0])
	if !ok || !state.Escalated {
		t.Fatalf("sticky comment %q -> state=%+v ok=%v, want the escalated state", st.comments[0], state, ok)
	}
	if !strings.Contains(st.comments[0], "Parked until") {
		t.Fatalf("sticky comment %q, want the corrected message describing what actually happens (#716 design item 4)", st.comments[0])
	}
	if strings.Contains(st.comments[0], "no longer selected") {
		t.Fatalf("sticky comment %q still carries the old, false claim", st.comments[0])
	}
}

// TestGatherPRContextDigestShortCircuitsOnClearedLabel is #716 design item
// 2's acceptance: a human clears goobers:merge-escalated (so
// escalationStillBlocks no longer excludes the PR — its live label is gone)
// but the underlying diff genuinely has not changed since it was recorded at
// the last escalation. Rather than spend a cycle (checkout, and downstream
// rebase-pr/remediation-checkpoint) reproducing the exact same escalation,
// gather-pr-context recognizes the unchanged digest itself and bails as a
// clean no-work tick.
func TestGatherPRContextDigestShortCircuitsOnClearedLabel(t *testing.T) {
	const prBranch = "goobers/impl/cleared-410"
	origin, headSHA, baseSHA := initPRBranchOrigin(t, prBranch)

	mgr, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	wt, err := mgr.Create(t.Context(), worktree.CreateOptions{
		RepoURL: origin, RunID: "digest-probe", BaseRef: "main",
		Branch: "goobers/pr-remediation/digest-probe",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := checkoutExistingBranch(wt.Path, prBranch, ""); err != nil {
		t.Fatalf("checkout probe branch: %v", err)
	}
	actualDigest, err := diffDigest(wt.Path, baseSHA)
	if err != nil {
		t.Fatalf("diffDigest: %v", err)
	}
	_ = wt.Remove(t.Context(), worktree.RemoveOptions{})

	stateComment, err := remediationStateComment(remediationState{
		Escalated: true, EscalatedHeadSHA: headSHA, EscalatedBaseSHA: baseSHA,
		LastDiffDigest: actualDigest,
	})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	srv := gatherPRContextServer{
		owner: "your-org", repo: "your-repo",
		prNumber: 410, head: prBranch, base: "main",
		headSHA: headSHA, baseSHA: baseSHA,
		labels: []string{needsRemediationLabel}, // merge-escalated cleared by a human
		comments: []map[string]interface{}{
			{"id": 1, "user": map[string]string{"login": "goobers-bot"}, "body": stateComment, "created_at": "2026-07-16T00:00:00Z"},
		},
	}
	server := srv.start(t)

	prev := newGitHubProvider
	newGitHubProvider = mergePRTestServer{url: server.URL}.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = prev })

	wt2, err := mgr.Create(t.Context(), worktree.CreateOptions{
		RepoURL: origin, RunID: "run-410-cleared", BaseRef: "main",
		Branch: "goobers/pr-remediation/run-410-cleared",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = wt2.Remove(t.Context(), worktree.RemoveOptions{}) })

	instanceRoot := initDemo(t)
	t.Setenv("GOOBERS_RUN_ID", "run-410-cleared")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_REPO_PUSH", "test-token")
	t.Chdir(wt2.Path)

	code, stdout, stderr := runArgs(t, "gather-pr-context", instanceRoot)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "no work") {
		t.Fatalf("stdout = %q, want no-work — diff is unchanged since the recorded escalation despite the label being cleared", stdout)
	}
	if _, err := os.Stat(filepath.Join(wt2.Path, "pr-context.json")); err == nil {
		t.Fatal("pr-context.json was written, want the digest short-circuit to bail before producing downstream context")
	}
}
