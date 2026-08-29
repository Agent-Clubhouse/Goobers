package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

// TestUpRecoversExpiredClaimAtStartup is #131's daemon-side acceptance:
// `goobers up` sweeps the claim ledger for expired leases once at startup,
// before the scheduler admits new ticks (localscheduler.ClaimLedger.
// RecoverExpired's doc: "call once at daemon start... and periodically
// thereafter").
func TestUpRecoversExpiredClaimAtStartup(t *testing.T) {
	root := initDeterministicDemo(t)
	schedulerDir := filepath.Join(root, "scheduler")

	// Seed an already-expired claim via a fake clock in the past, with a
	// POSITIVE lease duration — not a negative one (issue #235 now makes
	// ClaimLedger.Claim reject leaseDuration<=0, so the old
	// Claim(..., -time.Minute) exploit for "already expired" no longer
	// works). ClaimedAt/ExpiresAt land in the past relative to the real
	// clock the daemon's own ledger (opened below with no clock override)
	// reads them back with, so they're still expired as far as the real
	// RecoverExpired pass this test exercises is concerned.
	past := time.Now().Add(-2 * time.Hour)
	seedLedger, err := localscheduler.OpenClaimLedger(
		filepath.Join(schedulerDir, claimLedgerFileName),
		localscheduler.WithLedgerClock(func() time.Time { return past }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := seedLedger.Claim("issue-1", "crashed-run", "implementation", time.Minute); err != nil || !ok {
		t.Fatalf("seed expired claim: ok=%v err=%v", ok, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)

	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- runUpContext(ctx, []string{root}, &stdout, &stderr) }()

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runUpContext did not return after ctx cancellation")
	}

	if !strings.Contains(stdout.String(), "recovered expired claim issue-1") {
		t.Fatalf("stdout = %q, want a mention of the recovered expired claim", stdout.String())
	}

	reopened, err := localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, held := reopened.Lookup("issue-1"); held {
		t.Fatal("expired claim should have been released")
	}
}

// TestUpRecoversExpiredClaimPeriodically proves the ticker path: a claim
// that expires WHILE the daemon is already running (not just at startup) is
// still recovered, on claimRecoverInterval's cadence.
func TestUpRecoversExpiredClaimPeriodically(t *testing.T) {
	root := initDeterministicDemo(t)
	schedulerDir := filepath.Join(root, "scheduler")

	prevInterval := claimRecoverInterval
	claimRecoverInterval = 50 * time.Millisecond
	t.Cleanup(func() { claimRecoverInterval = prevInterval })

	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	var stdout, stderr bytes.Buffer
	started := &daemonStartedWriter{started: make(chan struct{})}
	done := make(chan int, 1)
	go func() { done <- runUpContext(ctx, []string{root}, io.MultiWriter(&stdout, started), &stderr) }()

	select {
	case <-started.started:
	case code := <-done:
		t.Fatalf("runUpContext exited before startup: code = %d, stderr = %q", code, stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for daemon startup")
	}

	// Claim only after startup recovery has completed, so this lease can be
	// released only by the periodic ticker under test.
	if ok, _, err := ledger.Claim("issue-2", "live-run", "implementation", 100*time.Millisecond); err != nil || !ok {
		t.Fatalf("seed live claim: ok=%v err=%v", ok, err)
	}
	time.AfterFunc(500*time.Millisecond, cancel)

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runUpContext did not return after ctx cancellation")
	}

	// The periodic sweep deliberately never writes to stdout/stderr (they're
	// shared with the main goroutine's own writes for the daemon's whole
	// lifetime, and io.Writer implementations like *bytes.Buffer are not
	// safe for concurrent use) — assert on the actual ledger state it
	// produced instead of log text.
	reopened, err := localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, held := reopened.Lookup("issue-2"); held {
		t.Fatalf("expired claim should have been released by the periodic sweep; stdout = %q", stdout.String())
	}
	if strings.Contains(stdout.String(), "recovered expired claim issue-2") {
		t.Fatalf("periodic recovery changed stdout: %q", stdout.String())
	}

	events, err := journal.ReadInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	var sawSummary bool
	for _, event := range events {
		if event.Type == journal.EventClaimReleased && strings.Contains(event.Reason, "periodic recovery released 1 expired claim") {
			sawSummary = true
			break
		}
	}
	if !sawSummary {
		t.Fatalf("instance journal has no compact periodic recovery summary: %+v", events)
	}
}

func TestUpJournalsPeriodicClaimRecoveryError(t *testing.T) {
	root := initDeterministicDemo(t)
	schedulerDir := filepath.Join(root, "scheduler")

	prevInterval := claimRecoverInterval
	claimRecoverInterval = 20 * time.Millisecond
	t.Cleanup(func() { claimRecoverInterval = prevInterval })

	ctx, cancel := context.WithCancel(context.Background())
	started := &daemonStartedWriter{started: make(chan struct{})}
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- runUpContext(ctx, []string{root}, started, &stderr) }()

	select {
	case <-started.started:
	case code := <-done:
		t.Fatalf("runUpContext exited before startup: code = %d, stderr = %q", code, stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for daemon startup")
	}

	if err := os.WriteFile(filepath.Join(schedulerDir, claimLedgerFileName), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	event := waitForInstanceError(t, schedulerDir, "claim_recovery_failed")
	if !strings.Contains(event.Error.Message, "parse claim ledger") {
		t.Fatalf("claim recovery error = %q, want ledger parse detail", event.Error.Message)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runUpContext did not return after cancellation")
	}
	if strings.Contains(stderr.String(), "claim_recovery_failed") {
		t.Fatalf("periodic claim error leaked to stderr: %q", stderr.String())
	}
}

func TestSweepErrorReporterRateLimitsIdenticalConsecutiveErrors(t *testing.T) {
	dir := t.TempDir()
	log, _, err := journal.OpenInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	reporter := newSweepErrorReporter(log, "claim_recovery_failed")
	reporter.reportEvery = 3
	repeated := errors.New("ledger unavailable")
	for range 4 {
		reporter.report(repeated)
	}
	if got := countInstanceErrors(t, dir, "claim_recovery_failed"); got != 2 {
		t.Fatalf("reported identical errors = %d, want first and fourth ticks only", got)
	}

	reporter.report(errors.New("ledger corrupt"))
	reporter.report(nil)
	reporter.report(repeated)
	if got := countInstanceErrors(t, dir, "claim_recovery_failed"); got != 4 {
		t.Fatalf("reported errors after change/reset = %d, want both reported immediately", got)
	}
}

func waitForInstanceError(t *testing.T, schedulerDir, code string) journal.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		events, err := journal.ReadInstanceLog(schedulerDir)
		if err == nil {
			for _, event := range events {
				if event.Type == journal.EventError && event.Error != nil && event.Error.Code == code {
					return event
				}
			}
		}
		time.Sleep(10 * time.Millisecond) // Polling interval; the instance journal has no notification hook.
	}
	t.Fatalf("timed out waiting for instance-journal error %q", code)
	return journal.Event{}
}

func countInstanceErrors(t *testing.T, schedulerDir, code string) int {
	t.Helper()
	events, err := journal.ReadInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	for _, event := range events {
		if event.Type == journal.EventError && event.Error != nil && event.Error.Code == code {
			count++
		}
	}
	return count
}

// TestRenewLiveClaimsExtendsLeaseBeyondOriginalWindow is issue #2014's AC1+AC2:
// a run whose renewals keep landing (this test's stand-in for a daemon
// process that keeps calling renewLiveClaims on claimRecoverInterval for
// every runID its daemonRunnerRegistry is still tracking) survives arbitrarily
// far past its ORIGINAL lease window, and stays refused to a second claimant,
// even though DefaultClaimLease itself is now short (30m, not the old 6h)
// specifically because renewal — not a large static constant — is what now
// keeps a genuinely still-running claim alive.
func TestRenewLiveClaimsExtendsLeaseBeyondOriginalWindow(t *testing.T) {
	root := t.TempDir()
	l := instance.NewLayout(root)
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}

	start := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	fakeNow := start
	seedLedger, err := localscheduler.OpenClaimLedger(
		filepath.Join(l.SchedulerDir(), claimLedgerFileName),
		localscheduler.WithLedgerClock(func() time.Time { return fakeNow }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := seedLedger.Claim("issue-3", "long-run", "implementation", DefaultClaimLease); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}

	// Three renewal cycles, each past the PRIOR cycle's expiry — well beyond
	// DefaultClaimLease's own 30m window in total — mirroring how a real
	// implement->ci-poll run outlasts one lease many times over now that
	// renewal, not lease size, is what keeps it alive.
	for i := 1; i <= 3; i++ {
		fakeNow = fakeNow.Add(DefaultClaimLease - time.Minute)
		reopened, err := localscheduler.OpenClaimLedger(
			filepath.Join(l.SchedulerDir(), claimLedgerFileName),
			localscheduler.WithLedgerClock(func() time.Time { return fakeNow }),
		)
		if err != nil {
			t.Fatal(err)
		}
		if released, err := reopened.RecoverExpired(fakeNow); err != nil || len(released) != 0 {
			t.Fatalf("cycle %d: claim must survive recovery before its renewal lands: released=%v err=%v", i, released, err)
		}
		trackedProbe := localscheduler.TrackedRunLiveness(func() []string { return []string{"long-run"} })
		renewed, probeErr, err := renewLiveClaims(context.Background(), l, trackedProbe, DefaultClaimLease)
		if probeErr != nil || err != nil {
			t.Fatalf("cycle %d: renewLiveClaims: probeErr=%v err=%v", i, probeErr, err)
		}
		if len(renewed) != 1 || renewed[0].ItemID != "issue-3" {
			t.Fatalf("cycle %d: renewed = %+v, want issue-3 renewed exactly once", i, renewed)
		}
	}

	// Now well past 3x DefaultClaimLease from the original claim — the claim
	// must still be held, and still refuse a second claimant.
	final, err := localscheduler.OpenClaimLedger(
		filepath.Join(l.SchedulerDir(), claimLedgerFileName),
		localscheduler.WithLedgerClock(func() time.Time { return fakeNow }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if entry, held := final.Lookup("issue-3"); !held || entry.RunID != "long-run" {
		t.Fatalf("claim should still be held by long-run: %+v held=%v", entry, held)
	}
	if ok, holder, err := final.Claim("issue-3", "second-run", "implementation", DefaultClaimLease); err != nil || ok || holder != "long-run" {
		t.Fatalf("a second claimant must be refused while long-run keeps renewing: ok=%v holder=%s err=%v", ok, holder, err)
	}
}

// TestClaimNotRenewedIsReapedAfterShrunkLease is #2014's AC3 flip side: a
// claim NOT passed to renewLiveClaims — this test's stand-in for a run whose
// owning process crashed, so nothing is calling renewLiveClaims with its
// runID anymore — is still correctly reaped once past the new, much shorter
// DefaultClaimLease. Proves shrinking the lease from 6h to 30m didn't just
// move the goalposts: RecoverExpired's crash-detection actually got faster,
// not weaker, once a live run no longer needs a large static lease to
// survive on its own.
func TestClaimNotRenewedIsReapedAfterShrunkLease(t *testing.T) {
	root := t.TempDir()
	l := instance.NewLayout(root)
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}

	start := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	fakeNow := start
	seedLedger, err := localscheduler.OpenClaimLedger(
		filepath.Join(l.SchedulerDir(), claimLedgerFileName),
		localscheduler.WithLedgerClock(func() time.Time { return fakeNow }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := seedLedger.Claim("issue-4", "crashed-run", "implementation", DefaultClaimLease); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}

	// No renewLiveClaims call for "crashed-run" at all — just advance past
	// its lease, well under the OLD 6h default this would never have caught.
	fakeNow = start.Add(DefaultClaimLease + time.Minute)
	reopened, err := localscheduler.OpenClaimLedger(
		filepath.Join(l.SchedulerDir(), claimLedgerFileName),
		localscheduler.WithLedgerClock(func() time.Time { return fakeNow }),
	)
	if err != nil {
		t.Fatal(err)
	}
	released, err := reopened.RecoverExpired(fakeNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(released) != 1 || released[0].ItemID != "issue-4" {
		t.Fatalf("released = %+v, want issue-4 reaped as unrenewed", released)
	}
	if _, held := reopened.Lookup("issue-4"); held {
		t.Fatal("unrenewed claim should have been released")
	}
}
