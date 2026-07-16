package main

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

func TestPRSelectConcurrentRunsExactlyOneClaimsPR(t *testing.T) {
	root := initDemo(t)
	eligible := []providers.PullRequestSummary{{Number: 77}}
	type outcome struct {
		runID    string
		selected *providers.PullRequestSummary
		err      error
	}
	outcomes := make(chan outcome, 2)
	start := make(chan struct{})

	var wg sync.WaitGroup
	for _, runID := range []string{"merge-run-a", "merge-run-b"} {
		wg.Add(1)
		go func(runID string) {
			defer wg.Done()
			<-start
			selected, err := claimPullRequest(root, eligible, runID, "merge-review", time.Hour)
			outcomes <- outcome{runID: runID, selected: selected, err: err}
		}(runID)
	}
	close(start)
	wg.Wait()
	close(outcomes)

	winners := 0
	noWork := 0
	winnerRunID := ""
	for outcome := range outcomes {
		if outcome.err != nil {
			t.Fatalf("claim: %v", outcome.err)
		}
		if outcome.selected == nil {
			noWork++
			continue
		}
		if outcome.selected.Number != 77 {
			t.Fatalf("selected PR = #%d, want #77", outcome.selected.Number)
		}
		winners++
		winnerRunID = outcome.runID
	}
	if winners != 1 || noWork != 1 {
		t.Fatalf("winners = %d, no-work selectors = %d; want exactly one of each", winners, noWork)
	}

	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	entry, held := ledger.Lookup(pullRequestClaimKey(77))
	if !held || entry.RunID != winnerRunID || entry.Workflow != "merge-review" {
		t.Fatalf("persisted PR claim = (%+v, %v), want winner %q in merge-review", entry, held, winnerRunID)
	}
}

func TestPullRequestClaimSharedAcrossSelectorEntrypoints(t *testing.T) {
	const prNumber = 78
	const prBranch = "goobers/implementation/run-78"

	origin, headSHA, baseSHA := initPRBranchOrigin(t, prBranch)
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(prNumber, "Shared claim PR")
	server.addOpenPR(prNumber, prBranch, "main", headSHA, baseSHA, false, nil, nil)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-run")
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	t.Setenv("GOOBERS_CRED_REPO_PUSH", "test-token")

	t.Setenv("GOOBERS_RUN_ID", "merge-run")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Chdir(t.TempDir())
	if code, stdout, stderr := runArgs(t, "pr-select", root); code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	server.mu.Lock()
	server.prs[prNumber].labels = []string{needsRemediationLabel}
	server.mu.Unlock()

	t.Setenv("GOOBERS_RUN_ID", "blocked-remediation-run")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Chdir(t.TempDir())
	if code, stdout, stderr := runArgs(t, "gather-pr-context", root); code != 0 || !strings.Contains(stdout, "no work") {
		t.Fatalf("gather-pr-context blocked by pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	l := layoutFor(root)
	instanceLog, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := releaseClaimsForRun(l, instanceLog, "merge-run"); err != nil {
		t.Fatal(err)
	}
	if err := instanceLog.Close(); err != nil {
		t.Fatal(err)
	}

	mgr, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wt, err := mgr.Create(t.Context(), worktree.CreateOptions{
		RepoURL: origin,
		RunID:   "remediation-run",
		BaseRef: "main",
		Branch:  "goobers/pr-remediation/remediation-run",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wt.Remove(t.Context(), worktree.RemoveOptions{}) })

	t.Setenv("GOOBERS_RUN_ID", "remediation-run")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Chdir(wt.Path)
	if code, stdout, stderr := runArgs(t, "gather-pr-context", root); code != 0 {
		t.Fatalf("gather-pr-context: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	server.mu.Lock()
	server.prs[prNumber].labels = nil
	server.mu.Unlock()

	t.Setenv("GOOBERS_RUN_ID", "blocked-merge-run")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Chdir(t.TempDir())
	if code, stdout, stderr := runArgs(t, "pr-select", root); code != 0 || !strings.Contains(stdout, "no work") {
		t.Fatalf("pr-select blocked by gather-pr-context: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	entry, held := ledger.Lookup(pullRequestClaimKey(prNumber))
	if !held || entry.RunID != "remediation-run" {
		t.Fatalf("shared PR claim = (%+v, %v), want remediation-run", entry, held)
	}

	instanceLog, _, err = journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := releaseClaimsForRun(l, instanceLog, "remediation-run"); err != nil {
		t.Fatal(err)
	}
	if err := instanceLog.Close(); err != nil {
		t.Fatal(err)
	}

	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	acquired := 0
	released := 0
	for _, event := range events {
		if event.Name != pullRequestClaimKey(prNumber) {
			continue
		}
		switch event.Type {
		case journal.EventClaimAcquired:
			acquired++
		case journal.EventClaimReleased:
			released++
		}
	}
	if acquired != 2 || released != 2 {
		t.Fatalf("PR claim journal events: acquired = %d, released = %d; want 2 each", acquired, released)
	}
}

func TestPullRequestClaimExpiredLeaseIsSelectable(t *testing.T) {
	root := initDemo(t)
	l := layoutFor(root)
	ledgerPath := filepath.Join(l.SchedulerDir(), claimLedgerFileName)
	expiredClock := func() time.Time {
		return time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	ledger, err := localscheduler.OpenClaimLedger(
		ledgerPath,
		localscheduler.WithLedgerClock(expiredClock),
	)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim(
		pullRequestClaimKey(79),
		"expired-run",
		"merge-review",
		time.Minute,
	); err != nil || !ok {
		t.Fatalf("seed expired claim: ok = %v, err = %v", ok, err)
	}

	selected, err := claimPullRequest(
		root,
		[]providers.PullRequestSummary{{Number: 79}},
		"new-run",
		"pr-remediation",
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if selected == nil || selected.Number != 79 {
		t.Fatalf("selected = %+v, want expired PR #79 selectable again", selected)
	}

	reopened, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	entry, held := reopened.Lookup(pullRequestClaimKey(79))
	if !held || entry.RunID != "new-run" {
		t.Fatalf("replacement claim = (%+v, %v), want new-run", entry, held)
	}
}
