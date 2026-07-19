package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
)

func TestReleaseClaimsForRunReleasesAllOwnedClaims(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)
	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
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
