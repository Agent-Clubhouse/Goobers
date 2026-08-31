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
	"github.com/goobers/goobers/internal/workflow"
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
	holder, err := lock.TryAcquire(lockPath)
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
	var timeoutEvents []journal.Event
	for _, event := range events {
		if event.Type == journal.EventClaimLockTimeout {
			timeoutEvents = append(timeoutEvents, event)
		}
	}
	if len(timeoutEvents) != 1 || timeoutEvents[0].RunID != runID {
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
	released, err := recoverClaims(l, log, time.Now(), nil)
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
	released, err := recoverClaims(l, log, time.Now(), nil)
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

// newLiveRun hand-constructs a run journal with no run.finished event, so its
// reconstructed phase is journal.PhaseRunning — a live holder to hold
// conservatively against, as opposed to newStaleTerminalRun's terminal one.
func newLiveRun(t *testing.T, l instance.Layout, runID, workflowName string) {
	t.Helper()
	set, report, err := instance.LoadConfigDir(l.ConfigDir())
	if err != nil {
		t.Fatalf("load fixture config: %v (report: %+v)", err, report)
	}
	var gaggle, digest string
	found := false
	for i := range set.Workflows {
		if set.Workflows[i].Name == workflowName {
			m, err := workflow.Compile(workflow.Definition{Name: set.Workflows[i].Name, Version: 1, DSLVersion: set.Workflows[i].DSLVersion, Spec: set.Workflows[i].Spec}, workflow.WithPreviewFeatures(true))
			if err != nil {
				t.Fatalf("compile fixture workflow: %v", err)
			}
			gaggle = set.Workflows[i].Spec.Gaggle
			digest = m.Digest()
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("workflow %q not found in fixture config", workflowName)
	}
	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: workflowName, WorkflowVersion: 1,
		WorkflowDigest: digest, Gaggle: gaggle,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("hand-construct live run journal: %v", err)
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestRecoverClaimsResolvesBacklogReconcileClaimsByOwningRun covers the
// backlogreconcile.go/claims.go half of the askpass-relative-residual
// cluster: a synthesized backlog-reconcile claim RunID ("<owner-run>/
// backlog-reconcile/<pid>/<seq>") is not itself a run FindRunDir can look up
// (it contains "/"), so claimHolderTerminal must recover the owning run's
// id and inspect that instead — terminal owner releases, live owner holds,
// and a shape that fails to parse holds conservatively without spamming a
// terminal_claim_inspection_failed event on every recovery sweep (#found:
// 64 occurrences/hour in production before this fix).
func TestRecoverClaimsResolvesBacklogReconcileClaimsByOwningRun(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	const (
		terminalOwner = "reconcile-owner-terminal"
		liveOwner     = "reconcile-owner-live"
	)
	newStaleTerminalRun(t, l, terminalOwner, "default-implement", journal.PhaseCompleted, "local-ci")
	newLiveRun(t, l, liveOwner, "default-implement")

	terminalClaim := formatBacklogReconcileRunID(terminalOwner, 555, 1)
	liveClaim := formatBacklogReconcileRunID(liveOwner, 555, 2)
	malformedClaim := terminalOwner + "/" + backlogReconcileRunIDComponent + "/not-a-pid/3"

	ledgerPath := filepath.Join(l.SchedulerDir(), claimLedgerFileName)
	ledger, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	for itemID, runID := range map[string]string{
		"700": terminalClaim,
		"701": liveClaim,
		"702": malformedClaim,
	} {
		if ok, _, err := ledger.Claim(itemID, runID, "backlog-reconcile", time.Hour); err != nil || !ok {
			t.Fatalf("seed claim %s (run %s): ok=%v err=%v", itemID, runID, ok, err)
		}
	}

	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	released, err := recoverClaims(l, log, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if len(released) != 1 || released[0].ItemID != "700" {
		t.Fatalf("released = %+v, want only the terminal-owner reconcile claim (700)", released)
	}

	reopened, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, held := reopened.Lookup("700"); held {
		t.Fatal("reconcile claim owned by a terminal run survived recovery")
	}
	if entry, held := reopened.Lookup("701"); !held || entry.RunID != liveClaim {
		t.Fatalf("reconcile claim owned by a live run changed: (%+v, %v)", entry, held)
	}
	if entry, held := reopened.Lookup("702"); !held || entry.RunID != malformedClaim {
		t.Fatalf("malformed reconcile-shaped claim changed: (%+v, %v)", entry, held)
	}

	instanceEvents, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range instanceEvents {
		if event.Error != nil && event.Error.Code == "terminal_claim_inspection_failed" {
			t.Fatalf("unexpected terminal_claim_inspection_failed event for a reconcile-format run id: %+v", event)
		}
	}
}

// TestRecoverClaimsReleasesTerminalBacklogScopedClaim is the currentClaimEntry
// regression: a v3 backlog-scoped lease (personal-gaggle-routing §5.3) is
// stored under the backlog key, so re-reading it under a hand-rebuilt
// gaggle/provider key found nothing, recovery concluded "no longer held", and
// the lease of an already-terminal run was never released — it sat held until
// its lease expired, and the run's PR-remediation no-op was never recorded.
//
// The gaggle-scoped claim alongside it proves the legacy dispatch still works
// and that fixing the backlog lookup did not start releasing the wrong entry.
func TestRecoverClaimsReleasesTerminalBacklogScopedClaim(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	const (
		terminalRun = "backlog-scoped-terminal-holder"
		liveRun     = "backlog-scoped-live-holder"
	)
	newStaleTerminalRun(t, l, terminalRun, "default-implement", journal.PhaseCompleted, "local-ci")
	newLiveRun(t, l, liveRun, "default-implement")

	identity := apiv1.BacklogIdentity{
		Provider: apiv1.ProviderGitHub, Owner: "gim-home", Name: "brandiv.goobers",
	}
	if err := identity.Validate(); err != nil {
		t.Fatalf("fixture backlog identity: %v", err)
	}
	terminalKey := backlogClaimKey(identity, "example", "820")
	liveKey := backlogClaimKey(identity, "example", "821")

	ledgerPath := filepath.Join(l.SchedulerDir(), claimLedgerFileName)
	ledger, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.ClaimScoped(terminalKey, terminalRun, "default-implement", time.Hour); err != nil || !ok {
		t.Fatalf("seed backlog-scoped terminal claim: ok=%v err=%v", ok, err)
	}
	if ok, _, err := ledger.ClaimScoped(liveKey, liveRun, "default-implement", time.Hour); err != nil || !ok {
		t.Fatalf("seed backlog-scoped live claim: ok=%v err=%v", ok, err)
	}
	gaggleKey := localscheduler.ClaimKey{Gaggle: "example", Provider: "github", ExternalID: "822"}
	if ok, _, err := ledger.ClaimScoped(gaggleKey, terminalRun, "default-implement", time.Hour); err != nil || !ok {
		t.Fatalf("seed gaggle-scoped terminal claim: ok=%v err=%v", ok, err)
	}

	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	released, err := recoverClaims(l, log, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	releasedItems := make(map[string]bool, len(released))
	for _, entry := range released {
		if entry.RunID != terminalRun {
			t.Fatalf("recovery released a claim held by %q: %+v", entry.RunID, entry)
		}
		releasedItems[entry.ItemID] = true
	}
	if !releasedItems["820"] || !releasedItems["822"] || len(releasedItems) != 2 {
		t.Fatalf("released items = %v, want the terminal run's backlog-scoped (820) and gaggle-scoped (822) claims", releasedItems)
	}

	reopened, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if entry, held := reopened.LookupScoped(terminalKey); held {
		t.Fatalf("backlog-scoped claim of a terminal run survived recovery: %+v", entry)
	}
	if entry, held := reopened.LookupScoped(gaggleKey); held {
		t.Fatalf("gaggle-scoped claim of a terminal run survived recovery: %+v", entry)
	}
	if entry, held := reopened.LookupScoped(liveKey); !held || entry.RunID != liveRun {
		t.Fatalf("live run's backlog-scoped claim = (%+v, %v), want preserved", entry, held)
	}
}
