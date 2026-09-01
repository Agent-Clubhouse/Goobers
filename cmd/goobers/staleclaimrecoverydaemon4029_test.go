package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

// staleclaimrecoverydaemon4029_test.go is Goobers#4029's evidence — the half
// of the stale-claim sweep #4016 deliberately left alone.
//
// #4016 moved the POD arm of `backlog-query --reconcile`'s opening sweep onto
// the claims plane, so a pod asks the daemon to run the daemon's own sweep.
// The SELF-placed arm kept calling recoverClaims(l, nil, now, nil, nil)
// directly, and those two nils are the defect: interventionActive and the
// restart-time RecoveryGate are in-memory daemon state, so a stage subprocess
// sweeping on its own reaps leases the daemon is deliberately holding.
//
// The fix is one predicate — does a daemon own this instance's up.lock? —
// because that is exactly the predicate for whether the two missing inputs
// exist at all:
//
//   - daemon holds it: DELEGATE, over the pending-claims channel `goobers
//     claims release` already uses, and the daemon answers with the same
//     recoverExpiredClaims closure its startup pass, its five-minute ticker
//     and the claims plane's /claims/recover all call.
//   - nobody, or a manual `goobers run`/`signal` holder: no intervention
//     service and no gate exist in the first place, so nil and nil are the
//     truth. Sweep in process, byte-identical to before.
//
// Each test below pins one of those arms, and the two "honours" tests assert
// the gap directly: the SAME ledger state that the daemon's sweep correctly
// preserves is destroyed by the in-process sweep the self arm used to run.

const sweepFixtureGaggle = "example"

// daemonInstance is a self-placed stage's world: a real instance root, a real
// claim ledger, and (optionally) a live daemon holding up.lock and servicing
// the pending-claims delegation channel with a sweep of the test's choosing.
type daemonInstance struct {
	layout instance.Layout
	log    *journal.InstanceLog
	sweeps atomic.Int64
}

func newDaemonInstance(t *testing.T) *daemonInstance {
	t.Helper()
	// Off the plane: this whole file is about the FILE arm of the seam.
	t.Setenv(claimsclient.EnvEndpoint, "")
	t.Setenv(claimsclient.EnvToken, "")

	layout := layoutFor(initDemo(t))
	instanceLog, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instanceLog.Close() })
	return &daemonInstance{layout: layout, log: instanceLog}
}

func (d *daemonInstance) lockPath() string {
	return filepath.Join(d.layout.SchedulerDir(), "up.lock")
}

// startDaemon takes the DAEMON-kind instance lock and services the
// pending-claims channel until the test ends, answering recovery requests
// with sweep — standing in for up.go's delegation ticker, whose only input
// this test needs to vary is the closure.
func (d *daemonInstance) startDaemon(t *testing.T, sweep daemonStaleClaimSweep) {
	t.Helper()
	release, err := acquireDaemonLock(d.lockPath(), d.layout.Root, time.Minute, nil)
	if err != nil {
		t.Fatalf("acquire daemon lock: %v", err)
	}
	t.Cleanup(release)

	counted := func(now time.Time) ([]localscheduler.ClaimEntry, error) {
		d.sweeps.Add(1)
		return sweep(now)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	t.Cleanup(func() {
		close(stop)
		<-done
	})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
				_ = sweepPendingClaimAdminRequests(d.layout.SchedulerDir(), d.log, time.Now, counted)
			}
		}
	}()
}

// daemonSweep is recoverExpiredClaims as up.go builds it: the daemon's own
// journal, its intervention predicate and its recovery gate.
func (d *daemonInstance) daemonSweep(interventionActive func(string) bool, gate *localscheduler.RecoveryGate) daemonStaleClaimSweep {
	return func(now time.Time) ([]localscheduler.ClaimEntry, error) {
		return recoverClaims(d.layout, d.log, now, interventionActive, gate)
	}
}

// seedExpiredLease puts a lease on the ledger that is already past its
// expiry, so only RecoverExpired removes it.
func (d *daemonInstance) seedExpiredLease(t *testing.T, itemID, runID string) {
	t.Helper()
	past := time.Now().Add(-2 * time.Hour)
	ledger, err := localscheduler.OpenClaimLedger(
		filepath.Join(d.layout.SchedulerDir(), claimLedgerFileName),
		localscheduler.WithLedgerClock(func() time.Time { return past }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim(itemID, runID, "implementation", time.Minute); err != nil || !ok {
		t.Fatalf("seed expired lease: ok=%v err=%v", ok, err)
	}
}

// seedTerminalHolderLease puts a LIVE (unexpired) lease on the ledger whose
// owning run has already reached a terminal phase. Only the terminal half of
// recoverClaims releases that — and the terminal half is the half
// interventionActive guards.
func (d *daemonInstance) seedTerminalHolderLease(t *testing.T, itemID, runID string) {
	t.Helper()
	run, err := journal.Create(
		d.layout.ForGaggle(sweepFixtureGaggle).RunsDir(),
		journal.RunIdentity{RunID: runID, Workflow: "backlog-curation", Gaggle: sweepFixtureGaggle},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{
		Type:   journal.EventRunFinished,
		Status: string(journal.PhaseCompleted),
	}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(d.layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim(itemID, runID, "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed terminal-holder lease: ok=%v err=%v", ok, err)
	}
}

func (d *daemonInstance) leaseHeld(t *testing.T, itemID string) bool {
	t.Helper()
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(d.layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	_, held := ledger.Lookup(itemID)
	return held
}

// assertTerminalHolderRunIsReapable proves the lease seeded by
// seedTerminalHolderLease really is one the sweep would release, so a later
// "it survived" assertion is evidence about the GUARD and not about a lease
// that was never a candidate.
func (d *daemonInstance) assertTerminalHolderRunIsReapable(t *testing.T, itemID string) {
	t.Helper()
	released, err := recoverClaims(d.layout, d.log, time.Now(), nil, nil)
	if err != nil {
		t.Fatalf("control sweep: %v", err)
	}
	if d.leaseHeld(t, itemID) {
		t.Fatalf("control sweep released %d lease(s) but %s survived; the fixture is not a reap candidate", len(released), itemID)
	}
}

// TestSelfPlacedSweepDelegatesToALiveDaemon is the shape of the fix: with a
// daemon holding up.lock, the stage's sweep is answered by the DAEMON's
// closure, not run in the stage's own process.
func TestSelfPlacedSweepDelegatesToALiveDaemon(t *testing.T) {
	d := newDaemonInstance(t)
	d.seedExpiredLease(t, "issue-1", "expired-run")
	d.startDaemon(t, d.daemonSweep(nil, nil))

	if err := recoverStageClaims(d.layout, time.Now()); err != nil {
		t.Fatalf("recoverStageClaims under a live daemon: %v", err)
	}
	if got := d.sweeps.Load(); got == 0 {
		t.Fatal("the daemon's sweep was never invoked; the stage swept in its own process")
	}
	if d.leaseHeld(t, "issue-1") {
		t.Fatal("the expired lease survived the delegated sweep")
	}
}

// TestSelfPlacedSweepUnderALiveDaemonHonoursAnActiveIntervention is #4029's
// first regression. A terminal run's lease held under an ACTIVE intervention
// must survive: the daemon is re-driving that run and will re-acquire its
// claims. The control sweep first proves the same lease is otherwise reaped,
// so this asserts the guard rather than an inert fixture.
func TestSelfPlacedSweepUnderALiveDaemonHonoursAnActiveIntervention(t *testing.T) {
	control := newDaemonInstance(t)
	control.seedTerminalHolderLease(t, "issue-7", "intervened-run")
	control.assertTerminalHolderRunIsReapable(t, "issue-7")

	d := newDaemonInstance(t)
	d.seedTerminalHolderLease(t, "issue-7", "intervened-run")
	intervened := func(runID string) bool { return runID == "intervened-run" }
	d.startDaemon(t, d.daemonSweep(intervened, nil))

	if err := recoverStageClaims(d.layout, time.Now()); err != nil {
		t.Fatalf("recoverStageClaims under a live daemon: %v", err)
	}
	if !d.leaseHeld(t, "issue-7") {
		t.Fatal("the stage's sweep released a claim held under an active intervention")
	}
	if d.sweeps.Load() == 0 {
		t.Fatal("the daemon's sweep was never invoked")
	}
}

// TestSelfPlacedSweepUnderALiveDaemonRespectsAClosedRecoveryGate is #4029's
// second regression, and DS6's ordering rule. While the gate is closed — a
// daemon that has restarted but not yet rebuilt its renewal set — no reap may
// run, or a live distributed run's lease is reaped in the gap before
// renewals flow again. A stage passing a nil gate sweeps straight through it.
func TestSelfPlacedSweepUnderALiveDaemonRespectsAClosedRecoveryGate(t *testing.T) {
	d := newDaemonInstance(t)
	d.seedExpiredLease(t, "issue-3", "restarting-run")
	gate := localscheduler.NewRecoveryGate()
	d.startDaemon(t, d.daemonSweep(nil, gate))

	if err := recoverStageClaims(d.layout, time.Now()); err != nil {
		t.Fatalf("recoverStageClaims with the gate closed: %v", err)
	}
	if !d.leaseHeld(t, "issue-3") {
		t.Fatal("the stage's sweep reaped an expired-looking lease before the renewal set was rebuilt")
	}

	// The gate opening is the daemon's own renewal rebuild completing; the
	// very next delegated sweep is then permitted, so nothing is lost — only
	// deferred, which is the entire point of the gate.
	gate.MarkRenewalRebuilt()
	if err := recoverStageClaims(d.layout, time.Now()); err != nil {
		t.Fatalf("recoverStageClaims after the gate opened: %v", err)
	}
	if d.leaseHeld(t, "issue-3") {
		t.Fatal("the lease survived a permitted sweep")
	}
}

// TestSelfPlacedSweepWithNoDaemonSweepsInProcess pins the unchanged arm. No
// lock holder means no intervention service and no gate exist anywhere, so
// nil and nil are the truth and the stage is the only thing that can sweep.
func TestSelfPlacedSweepWithNoDaemonSweepsInProcess(t *testing.T) {
	d := newDaemonInstance(t)
	d.seedExpiredLease(t, "issue-2", "expired-run")

	if err := recoverStageClaims(d.layout, time.Now()); err != nil {
		t.Fatalf("recoverStageClaims with no daemon: %v", err)
	}
	if d.leaseHeld(t, "issue-2") {
		t.Fatal("the expired lease survived the in-process sweep")
	}
	if _, err := os.Stat(filepath.Join(d.layout.SchedulerDir(), pendingClaimsDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a sweep with no daemon wrote to the delegation channel (%v); nobody would ever answer it", err)
	}
}

// TestSelfPlacedSweepUnderAManualLockHolderSweepsInProcess is the case that
// makes the predicate "a DAEMON owns it" rather than "the lock is held".
// `goobers run` and `goobers signal` hold the same up.lock for the length of
// a one-shot, as a MANUAL holder — and they construct no intervention service
// and service no delegation channel, so a stage delegating to one would hang
// until the timeout. They are the mode-1 shape the nil arguments describe.
func TestSelfPlacedSweepUnderAManualLockHolderSweepsInProcess(t *testing.T) {
	d := newDaemonInstance(t)
	d.seedExpiredLease(t, "issue-4", "expired-run")

	release, err := acquireInstanceLock(d.lockPath())
	if err != nil {
		t.Fatalf("acquire manual instance lock: %v", err)
	}
	defer release()

	if err := recoverStageClaims(d.layout, time.Now()); err != nil {
		t.Fatalf("recoverStageClaims under a manual lock holder: %v", err)
	}
	if d.leaseHeld(t, "issue-4") {
		t.Fatal("the expired lease survived the in-process sweep")
	}
}

// TestSelfPlacedSweepSurfacesADelegationFailure keeps the failure loud. A
// daemon that holds the lock but never answers must produce an error naming
// the delegation, not a silent skip that lets the reconcile proceed as though
// the ledger were fresh.
func TestSelfPlacedSweepSurfacesADelegationFailure(t *testing.T) {
	previous := claimAdminDelegationTimeout
	claimAdminDelegationTimeout = 150 * time.Millisecond
	t.Cleanup(func() { claimAdminDelegationTimeout = previous })

	d := newDaemonInstance(t)
	release, err := acquireDaemonLock(d.lockPath(), d.layout.Root, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	err = recoverStageClaims(d.layout, time.Now())
	if err == nil {
		t.Fatal("an unanswered delegation reported a successful sweep")
	}
	if !strings.Contains(err.Error(), "delegate claim recovery to the running daemon") {
		t.Fatalf("err = %v, want it to name the delegation", err)
	}
}

// TestDelegatedStaleClaimSweepRefusesWithoutTheDaemonClosure mirrors
// daemonClaimService.Recover's nil-closure refusal on the plane: an assembly
// with no sweep to offer says so, rather than substituting a weaker one.
func TestDelegatedStaleClaimSweepRefusesWithoutTheDaemonClosure(t *testing.T) {
	if _, err := executeDelegatedStaleClaimSweep(nil, time.Now()); err == nil {
		t.Fatal("a nil daemon sweep answered a recovery request")
	}
}

// TestDelegatedStaleClaimSweepSwallowsAClaimsLockTimeout matches every other
// call site of the daemon's sweep: a pass that could not take the claims lock
// is deferred work already journaled by the lock helper, not a failure to
// report back to the delegating stage.
func TestDelegatedStaleClaimSweepSwallowsAClaimsLockTimeout(t *testing.T) {
	timeout := &journaledClaimsLockTimeoutError{
		timeout: &claimsLockTimeoutError{Operation: claimLockOperationRecovery, WaitDuration: time.Second},
	}
	resp, err := executeDelegatedStaleClaimSweep(func(time.Time) ([]localscheduler.ClaimEntry, error) {
		return nil, timeout
	}, time.Now())
	if err != nil {
		t.Fatalf("a journaled claims-lock timeout was surfaced: %v", err)
	}
	if len(resp.Recovered) != 0 {
		t.Fatalf("recovered = %+v, want none", resp.Recovered)
	}
}

// TestDelegatedStaleClaimSweepReportsWhatItReleased keeps the wire answer
// honest: the daemon reports the entries its sweep released, on the field
// that means "a recovery sweep did this" rather than reusing a list result.
func TestDelegatedStaleClaimSweepReportsWhatItReleased(t *testing.T) {
	d := newDaemonInstance(t)
	d.seedExpiredLease(t, "issue-5", "expired-run")

	requestID, err := writeClaimAdminRequest(d.layout.SchedulerDir(), claimAdminRequest{
		Operation: claimAdminOperationRecover,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sweepPendingClaimAdminRequests(
		d.layout.SchedulerDir(), d.log, time.Now, d.daemonSweep(nil, nil),
	); err != nil {
		t.Fatal(err)
	}
	resp, err := pollClaimAdminResponse(context.Background(), d.layout.SchedulerDir(), requestID, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Error != "" {
		t.Fatalf("response error = %q", resp.Error)
	}
	if len(resp.Recovered) != 1 || resp.Recovered[0].ItemID != "issue-5" {
		t.Fatalf("recovered = %+v, want the one expired lease", resp.Recovered)
	}
	if len(resp.Entries) != 0 || resp.Released != nil {
		t.Fatalf("a recovery response used the list/release fields: %+v", resp)
	}
}
