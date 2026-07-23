package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// TestIssueCloseOutCommentsClosesAndReleasesClaim is #132's issue-close-out
// CLI-level acceptance: invoking `goobers issue-close-out` via the actual
// CLI entrypoint recovers which item its own run claimed (from the claim
// ledger — issue-close-out has no other way to learn it, several stages and
// worktrees after backlog-query), finds the run's PR by its stable branch
// name, comments + marks the issue done, and releases the claim early
// instead of waiting for its lease to expire.
func TestIssueCloseOutCommentsClosesAndReleasesClaim(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Fix the bug", "goobers:approved", "goobers:ready")

	const runID = "run-1"
	const workflow = "implementation"

	// Seed the claim ledger as if backlog-query already claimed item 7 for
	// this run (its own worktree — and claimed-item.json in it — is long
	// gone by the time issue-close-out runs).
	schedulerDir := filepath.Join(root, "scheduler")
	if err := (func() error {
		ledger, err := localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, claimLedgerFileName))
		if err != nil {
			return err
		}
		_, _, err = ledger.Claim("7", runID, workflow, time.Hour)
		return err
	})(); err != nil {
		t.Fatalf("seed claim ledger: %v", err)
	}

	// Seed an open PR on the run's deterministic branch, as open-pr would
	// have created it.
	head := providers.BranchName(workflow, runID)
	server.mu.Lock()
	server.prs[1] = &fakePR{number: 1, title: "Implementation", head: head, base: "main", state: "open"}
	server.nextPR = 2
	server.mu.Unlock()

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", runID)
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "issue-close-out", root)
	if code != 0 {
		t.Fatalf("issue-close-out: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "closed out 7") {
		t.Fatalf("stdout = %q, want a mention of the closed-out item", stdout)
	}

	server.mu.Lock()
	issue := server.issues[7]
	server.mu.Unlock()
	if issue.state != "closed" {
		t.Fatalf("issue state = %q, want closed", issue.state)
	}
	if len(issue.comments) != 1 || !strings.Contains(issue.comments[0], "https://example/pull/1") {
		t.Fatalf("issue comments = %+v, want exactly one linking pull/1", issue.comments)
	}

	// The claim was released, not left to expire.
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, claimLedgerFileName))
	if err != nil {
		t.Fatalf("open claim ledger: %v", err)
	}
	if _, ok := ledger.ForRun(runID); ok {
		t.Fatal("expected the claim to be released after close-out")
	}
}

// TestIssueCloseOutReleasesClaimedLabel is #414's core acceptance: the
// goobers:claimed label a prior backlog-query --claim call wrote must be
// removed on the same close-out event that releases the ledger claim, not
// left to survive indefinitely — UpdateWorkItemStatus only ever swaps
// goobers/status:-prefixed labels, so without this fix the marker never
// clears. Covers both the done and in-review status branches: the ledger
// claim releases unconditionally in both, so the label must too.
func TestIssueCloseOutReleasesClaimedLabel(t *testing.T) {
	for _, status := range []string{"", "in-review"} {
		t.Run("status="+status, func(t *testing.T) {
			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			server.addIssue(7, "Fix the bug", "goobers:approved", "goobers:ready", "goobers:claimed")

			const runID = "run-1"
			const workflow = "implementation"

			schedulerDir := filepath.Join(root, "scheduler")
			if err := (func() error {
				ledger, err := localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, claimLedgerFileName))
				if err != nil {
					return err
				}
				_, _, err = ledger.Claim("7", runID, workflow, time.Hour)
				return err
			})(); err != nil {
				t.Fatalf("seed claim ledger: %v", err)
			}

			head := providers.BranchName(workflow, runID)
			server.mu.Lock()
			server.prs[1] = &fakePR{number: 1, title: "Implementation", head: head, base: "main", state: "open"}
			server.nextPR = 2
			server.mu.Unlock()

			providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", runID)
			if status != "" {
				t.Setenv("GOOBERS_INPUT_STATUS", status)
			}
			t.Chdir(t.TempDir())

			code, _, stderr := runArgs(t, "issue-close-out", root)
			if code != 0 {
				t.Fatalf("issue-close-out: code = %d, stderr = %q", code, stderr)
			}

			server.mu.Lock()
			labels := append([]string{}, server.issues[7].labels...)
			server.mu.Unlock()
			for _, l := range labels {
				if l == "goobers:claimed" {
					t.Fatalf("issue labels = %+v, want goobers:claimed removed", labels)
				}
			}
		})
	}
}

// TestIssueCloseOutInReviewStatusDoesNotClose is #361/#355's regression:
// with status=in-review declared, close-out comments and applies the
// goobers/status:in-review label same as before, but must NOT close the
// GitHub issue — the work isn't done until the PR merges (goobers
// post-merge, run by merge-review, is what advances it to done). The claim
// still releases on the same PR-open timing as today (unchanged) — a
// durable GitHub label, not the ephemeral claim ledger, is what protects an
// in-review issue from being re-claimed while its PR is still cycling.
func TestIssueCloseOutInReviewStatusDoesNotClose(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Fix the bug", "goobers:approved", "goobers:ready")

	const runID = "run-1"
	const workflow = "implementation"

	schedulerDir := filepath.Join(root, "scheduler")
	if err := (func() error {
		ledger, err := localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, claimLedgerFileName))
		if err != nil {
			return err
		}
		_, _, err = ledger.Claim("7", runID, workflow, time.Hour)
		return err
	})(); err != nil {
		t.Fatalf("seed claim ledger: %v", err)
	}

	head := providers.BranchName(workflow, runID)
	server.mu.Lock()
	server.prs[1] = &fakePR{number: 1, title: "Implementation", head: head, base: "main", state: "open"}
	server.nextPR = 2
	server.mu.Unlock()

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", runID)
	t.Setenv("GOOBERS_INPUT_STATUS", "in-review")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "issue-close-out", root)
	if code != 0 {
		t.Fatalf("issue-close-out: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "in-review") {
		t.Fatalf("stdout = %q, want a mention of the in-review status", stdout)
	}

	server.mu.Lock()
	issue := server.issues[7]
	server.mu.Unlock()
	if issue.state != "open" {
		t.Fatalf("issue state = %q, want open (status=in-review must not close the issue)", issue.state)
	}
	found := false
	for _, l := range issue.labels {
		if l == "goobers/status:in-review" {
			found = true
		}
	}
	if !found {
		t.Fatalf("issue labels = %+v, want goobers/status:in-review", issue.labels)
	}
	if len(issue.comments) != 1 {
		t.Fatalf("issue comments = %+v, want exactly one", issue.comments)
	}

	// Claim-release timing is unchanged: still released at PR-open time.
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, claimLedgerFileName))
	if err != nil {
		t.Fatalf("open claim ledger: %v", err)
	}
	if _, ok := ledger.ForRun(runID); ok {
		t.Fatal("expected the claim to still be released after close-out, even with status=in-review")
	}
}

func TestIssueCloseOutNeedsHumanParksAndNextTickClaimsDifferentIssue(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Rejected implementation", "goobers:approved", "goobers:ready", "goobers:claimed")
	server.addIssue(8, "Next ready issue", "goobers:approved", "goobers:ready")

	const runID = "run-rejected"
	const reason = "The implementation weakens the fail-closed contract."
	schedulerDir := filepath.Join(root, "scheduler")
	if err := (func() error {
		ledger, err := localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, claimLedgerFileName))
		if err != nil {
			return err
		}
		_, _, err = ledger.Claim("7", runID, "implementation", time.Hour)
		return err
	})(); err != nil {
		t.Fatalf("seed claim ledger: %v", err)
	}

	run, err := journal.Create(layoutFor(root).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "implementation", WorkflowDigest: journal.Digest([]byte("workflow")),
		Gaggle: "goobers",
	}, nil)
	if err != nil {
		t.Fatalf("create journal: %v", err)
	}
	defer func() { _ = run.Close() }()
	verdictData, err := json.Marshal(apiv1.Verdict{
		Decision: apiv1.VerdictFail,
		Summary:  reason,
	})
	if err != nil {
		t.Fatalf("marshal verdict: %v", err)
	}
	verdictRef, err := run.RecordArtifact("verdict/review-1.json", verdictData)
	if err != nil {
		t.Fatalf("record verdict: %v", err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventGateEvaluated, Gate: "review", Verdict: string(apiv1.VerdictFail),
		Target: "park-needs-human", Name: "verdict/review-1.json", Ref: &verdictRef,
	}); err != nil {
		t.Fatalf("record review event: %v", err)
	}

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", runID)
	t.Setenv("GOOBERS_INPUT_STATUS", "needs-human")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "issue-close-out", root)
	if code != 0 {
		t.Fatalf("issue-close-out: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "parked 7 needs-human") {
		t.Fatalf("stdout = %q, want parked issue message", stdout)
	}

	server.mu.Lock()
	parked := server.issues[7]
	labels := append([]string{}, parked.labels...)
	comments := append([]string{}, parked.comments...)
	server.mu.Unlock()
	if !hasAnyLabel(labels, []string{providers.LabelNeedsHuman}) {
		t.Fatalf("issue labels = %v, want %s", labels, providers.LabelNeedsHuman)
	}
	if hasAnyLabel(labels, []string{providers.LabelReady, providers.LabelClaimed}) {
		t.Fatalf("issue labels = %v, want ready and claimed removed", labels)
	}
	if len(comments) != 1 || !strings.Contains(comments[0], reason) {
		t.Fatalf("issue comments = %v, want exactly one containing reviewer reason %q", comments, reason)
	}

	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, claimLedgerFileName))
	if err != nil {
		t.Fatalf("open claim ledger: %v", err)
	}
	if _, ok := ledger.ForRun(runID); ok {
		t.Fatal("expected rejected run's claim to be released")
	}

	t.Setenv("GOOBERS_RUN_ID", "run-next")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	t.Setenv("GOOBERS_INPUT_EXCLUDELABELS", "goobers/status:in-review")
	code, stdout, stderr = runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("next backlog-query: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "claimed 8") {
		t.Fatalf("next backlog-query stdout = %q, want issue 8 claimed instead of parked FIFO head", stdout)
	}
}

func TestIssueCloseOutGateReasonDescribesAutomatedEscalation(t *testing.T) {
	runsDir := t.TempDir()
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "run-local-ci", Workflow: "implementation", WorkflowDigest: journal.Digest([]byte("workflow")),
		Gaggle: "goobers",
	}, nil)
	if err != nil {
		t.Fatalf("create journal: %v", err)
	}
	defer func() { _ = run.Close() }()
	if err := run.Append(journal.Event{
		Type: journal.EventGateEvaluated, Gate: "local-gate", Verdict: "fail", Target: "park-needs-human",
		Runner: map[string]any{"escalated": true, "repassAttempt": 4},
	}); err != nil {
		t.Fatalf("record local gate event: %v", err)
	}

	reason, err := issueCloseOutReason(runsDir, "run-local-ci", "")
	if err != nil {
		t.Fatalf("issueCloseOutReason: %v", err)
	}
	if !strings.Contains(reason, "local-gate") || !strings.Contains(reason, "attempt 4") {
		t.Fatalf("reason = %q, want local-gate and repass attempt", reason)
	}
}

func TestIssueCloseOutReasonUsesNonRetryableTaskSummary(t *testing.T) {
	runsDir := t.TempDir()
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "run-over-scope", Workflow: "implementation", WorkflowDigest: journal.Digest([]byte("workflow")),
		Gaggle: "goobers",
	}, nil)
	if err != nil {
		t.Fatalf("create journal: %v", err)
	}
	defer func() { _ = run.Close() }()
	const summary = "The issue combines unrelated changes and must be decomposed."
	if err := run.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Status: string(apiv1.ResultFailure),
		Error: &journal.ErrorDetail{Code: "NEEDS_DECOMPOSITION", Message: summary},
	}); err != nil {
		t.Fatalf("record implement failure: %v", err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "park-needs-human", Status: string(apiv1.ResultFailure),
		AttemptClass: journal.AttemptInfra,
		Error:        &journal.ErrorDetail{Code: "interrupted", Message: "attempt was interrupted"},
	}); err != nil {
		t.Fatalf("record interrupted parking attempt: %v", err)
	}

	reason, err := issueCloseOutReason(runsDir, "run-over-scope", "")
	if err != nil {
		t.Fatalf("issueCloseOutReason: %v", err)
	}
	if reason != summary {
		t.Fatalf("reason = %q, want task summary %q", reason, summary)
	}
}

// TestIssueCloseOutRejectsUnknownStatus proves a typo'd status input fails
// closed rather than silently defaulting to done, in-review, or needs-human.
func TestIssueCloseOutRejectsUnknownStatus(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Fix the bug", "goobers:approved", "goobers:ready")

	const runID = "run-1"
	schedulerDir := filepath.Join(root, "scheduler")
	if err := (func() error {
		ledger, err := localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, claimLedgerFileName))
		if err != nil {
			return err
		}
		_, _, err = ledger.Claim("7", runID, "implementation", time.Hour)
		return err
	})(); err != nil {
		t.Fatalf("seed claim ledger: %v", err)
	}

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", runID)
	t.Setenv("GOOBERS_INPUT_STATUS", "definitely-not-a-real-status")
	t.Chdir(t.TempDir())

	code, _, stderr := runArgs(t, "issue-close-out", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (fail closed on an unknown status), stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "unsupported status") {
		t.Fatalf("stderr = %q, want a clear unsupported-status message", stderr)
	}
}

// TestIssueCloseOutNoClaimInLedgerFailsClosed proves issue-close-out errors
// clearly when the claim ledger holds no entry for its run — it has no other
// way to know which item to comment on/close, so it must not guess or no-op
// silently.
// TestIssueCloseOutNoLiveClaimIsResumeNoOp: with no claim in the ledger for
// this run, close-out no longer fails closed — an absent claim means a prior
// close-out attempt already ran through its comment + mark-done + release (the
// release is close-out's last step), so resuming succeeds as a no-op instead of
// failing the run at its final stage (#241 flipped the earlier fail-closed
// behavior; the resume/no-re-comment guarantees are asserted in
// prchainfinish241_test.go).
func TestIssueCloseOutNoLiveClaimIsResumeNoOp(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Fix the bug", "goobers:approved", "goobers:ready")

	const runID = "run-1"
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", runID)
	t.Chdir(t.TempDir())

	// No claim seeded in the ledger for run-1 (a prior attempt released it).
	code, stdout, stderr := runArgs(t, "issue-close-out", root)
	if code != 0 {
		t.Fatalf("code = %d, want 0 (resume no-op), stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "already released") {
		t.Fatalf("stdout = %q, want an already-released no-op note", stdout)
	}
	// A no-op must not re-touch the issue (no duplicate comment on resume).
	server.mu.Lock()
	comments := len(server.issues[7].comments)
	server.mu.Unlock()
	if comments != 0 {
		t.Fatalf("no-op close-out must not re-comment; got %d comment(s)", comments)
	}
}

// TestIssueCloseOutMissingRunIDFailsClosed proves issue-close-out refuses to
// run without a real run identity.
func TestIssueCloseOutMissingRunIDFailsClosed(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")

	prev := newGitHubProvider
	newGitHubProvider = server.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = prev })
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	// #321: a live local-ci `go test ./...` inherits the run's real
	// GOOBERS_RUN_ID/GOOBERS_WORKFLOW from buildStageEnv, defeating this
	// fail-closed test. Simulate the parent-process leak, then clear it —
	// genuinely exercises the missing-run-context path and regression-guards the
	// fix under normal CI.
	t.Setenv("GOOBERS_RUN_ID", "ambient-parent-leak")
	t.Setenv("GOOBERS_WORKFLOW", "ambient-parent-leak")
	unsetRunContext(t)
	t.Chdir(t.TempDir())

	code, _, stderr := runArgs(t, "issue-close-out", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (fail closed on missing run context), stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "GOOBERS_RUN_ID") {
		t.Fatalf("stderr = %q, want a clear missing-run-id message", stderr)
	}
}
