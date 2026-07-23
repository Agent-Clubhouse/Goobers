package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
)

func TestReleaseClaimsForRunReleasesAllOwnedClaims(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)
	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	ledgerPath := filepath.Join(l.SchedulerDir(), claimLedgerFileName)
	ledger, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, itemID := range []string{"7", "8", "9"} {
		if ok, _, err := ledger.Claim(itemID, "terminal-run", "backlog-curation", time.Hour); err != nil || !ok {
			t.Fatalf("seed claim %s: ok=%v err=%v", itemID, ok, err)
		}
	}
	if ok, _, err := ledger.Claim("10", "other-run", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed other claim: ok=%v err=%v", ok, err)
	}

	if err := releaseClaimsForRun(l, log, "terminal-run"); err != nil {
		t.Fatal(err)
	}
	if err := releaseClaimsForRun(l, log, "terminal-run"); err != nil {
		t.Fatalf("idempotent release: %v", err)
	}

	reopened, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if entries := reopened.ForRunAll("terminal-run"); len(entries) != 0 {
		t.Fatalf("terminal run still holds claims: %+v", entries)
	}
	if entry, ok := reopened.Lookup("10"); !ok || entry.RunID != "other-run" {
		t.Fatalf("other run's claim = (%+v, %v), want preserved", entry, ok)
	}

	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var released int
	for _, event := range events {
		if event.Type == journal.EventClaimReleased && event.RunID == "terminal-run" {
			released++
		}
	}
	if released != 3 {
		t.Fatalf("claim.released events = %d, want 3", released)
	}
}

func TestRunAbortReleasesOwnedClaims(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	const runID = "stuck-with-claim"
	newStuckRun(t, l, runID, "default-implement")

	ledgerPath := filepath.Join(l.SchedulerDir(), claimLedgerFileName)
	ledger, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim("498", runID, "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}

	code, _, stderr := runArgs(t, "run", "abort", runID, root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}

	reopened, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if entry, ok := reopened.Lookup("498"); ok {
		t.Fatalf("aborted run's claim leaked: %+v", entry)
	}
}

func TestRunAbortRetryReleasesOwnedClaims(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	const runID = "already-aborted-with-claim"
	newStaleTerminalRun(t, l, runID, "default-implement", journal.PhaseAborted, "local-ci")

	ledgerPath := filepath.Join(l.SchedulerDir(), claimLedgerFileName)
	ledger, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim("498", runID, "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}

	code, _, stderr := runArgs(t, "run", "abort", runID, root)
	if code != 1 {
		t.Fatalf("code = %d, want already-terminal result, stderr = %q", code, stderr)
	}

	reopened, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if entry, ok := reopened.Lookup("498"); ok {
		t.Fatalf("already-terminal abort retry left claim held: %+v", entry)
	}
}

func TestConfiguredRunnerReleasesClaimsAtTerminal(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(context.Background(), l, &wg)
	if err != nil {
		t.Fatal(err)
	}
	defer setup.Shutdown(context.Background())

	const runID = "terminal-claim-run"
	ledgerPath := filepath.Join(l.SchedulerDir(), claimLedgerFileName)
	ledger, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim("498", runID, "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}

	res, err := setup.Runner.Start(context.Background(), runner.StartInput{
		RunID:   runID,
		Machine: setup.Machines[localscheduler.WorkflowIdentity{Gaggle: "example", Workflow: "default-implement"}],
		Gaggle:  "example",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: setup.RepoRefs[localscheduler.WorkflowIdentity{Gaggle: "example", Workflow: "default-implement"}],
		Item:    &apiv1.BacklogItem{ID: "498"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}

	reopened, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if entry, ok := reopened.Lookup("498"); ok {
		t.Fatalf("terminal run's claim leaked: %+v", entry)
	}
}

func TestTerminalClaimReleaseTimeoutDefersToRecoverySweep(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	cfg.RunConditions.ClaimsLockTimeout = "20ms"
	if err := instance.WriteConfig(l.ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}

	const runID = "terminal-lock-timeout"
	newStaleTerminalRun(t, l, runID, "default-implement", journal.PhaseCompleted, "local-ci")
	ledgerPath := filepath.Join(l.SchedulerDir(), claimLedgerFileName)
	ledger, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim("498", runID, "default-implement", time.Hour); err != nil || !ok {
		t.Fatalf("seed terminal claim: ok=%v err=%v", ok, err)
	}
	if ok, _, err := ledger.Claim("499", "live-run", "default-implement", time.Hour); err != nil || !ok {
		t.Fatalf("seed live claim: ok=%v err=%v", ok, err)
	}

	manager, err := worktree.NewManager(l.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(l.SchedulerDir(), claimLockFileName)
	holder, err := lock.Acquire(lockPath)
	if err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	if err := finalizeTerminalRun(l, nil, manager, runID); err != nil {
		t.Fatalf("terminal timeout should defer cleanup: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("terminal finalization took %s, want bounded near 20ms", elapsed)
	}
	reopened, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, held := reopened.Lookup("498"); !held {
		t.Fatal("timed-out finalizer released a claim without acquiring the lock")
	}
	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != journal.EventClaimLockTimeout || events[0].RunID != runID {
		t.Fatalf("deferred finalization event = %+v, want timeout attributed to %s", events, runID)
	}

	if err := holder.Release(); err != nil {
		t.Fatal(err)
	}
	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	released, err := recoverClaims(l, log, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(released) != 1 || released[0].RunID != runID {
		t.Fatalf("recovery released %+v, want terminal run claim", released)
	}

	reopened, err = localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if entry, held := reopened.Lookup("498"); held {
		t.Fatalf("terminal claim survived recovery: %+v", entry)
	}
	if entry, held := reopened.Lookup("499"); !held || entry.RunID != "live-run" {
		t.Fatalf("recovery changed live claim: (%+v, %v)", entry, held)
	}
}

func TestRecoverClaimsSkipsCorruptHolderJournal(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	const (
		corruptRun  = "corrupt-terminal-holder"
		terminalRun = "healthy-terminal-holder"
	)
	newStaleTerminalRun(t, l, corruptRun, "default-implement", journal.PhaseCompleted, "local-ci")
	newStaleTerminalRun(t, l, terminalRun, "default-implement", journal.PhaseCompleted, "local-ci")
	eventsPath := filepath.Join(l.RunsDir(), corruptRun, "events.jsonl")
	eventsFile, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eventsFile.WriteString("{not-json}\n"); err != nil {
		_ = eventsFile.Close()
		t.Fatal(err)
	}
	if err := eventsFile.Close(); err != nil {
		t.Fatal(err)
	}

	ledgerPath := filepath.Join(l.SchedulerDir(), claimLedgerFileName)
	ledger, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim("500", corruptRun, "default-implement", time.Hour); err != nil || !ok {
		t.Fatalf("seed corrupt holder claim: ok=%v err=%v", ok, err)
	}
	if ok, _, err := ledger.Claim("501", terminalRun, "default-implement", time.Hour); err != nil || !ok {
		t.Fatalf("seed terminal holder claim: ok=%v err=%v", ok, err)
	}

	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	released, err := recoverClaims(l, log, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if len(released) != 1 || released[0].RunID != terminalRun {
		t.Fatalf("released = %+v, want only healthy terminal holder", released)
	}

	reopened, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, held := reopened.Lookup("500"); !held {
		t.Fatal("corrupt holder claim was released without a terminal determination")
	}
	if entry, held := reopened.Lookup("501"); held {
		t.Fatalf("healthy terminal claim survived recovery: %+v", entry)
	}
	instanceEvents, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	foundInspectionError := false
	for _, event := range instanceEvents {
		if event.Error != nil && event.Error.Code == "terminal_claim_inspection_failed" && event.RunID == corruptRun {
			foundInspectionError = true
		}
	}
	if !foundInspectionError {
		t.Fatalf("instance events lack corrupt-holder inspection error: %+v", instanceEvents)
	}
}
