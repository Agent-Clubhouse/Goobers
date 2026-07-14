package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	// A short-but-live lease at startup (RecoverExpired's startup pass must
	// NOT touch it), expiring well within the test's run window so only the
	// periodic ticker recovers it.
	if ok, _, err := ledger.Claim("issue-2", "live-run", "implementation", 100*time.Millisecond); err != nil || !ok {
		t.Fatalf("seed live claim: ok=%v err=%v", ok, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(500*time.Millisecond, cancel)

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
}

// TestDefaultClaimLeaseSurvivesARealisticLongRun is issue #235 edge 2's
// acceptance test: the chosen fix is raising DefaultClaimLease comfortably
// above a realistic ci-poll-bearing run's duration, not liveness-aware
// RecoverExpired (deferred to V1 hardening, per ClaimLedger.RecoverExpired's
// own doc). This proves that choice actually closes the reachable window —
// a claim held for a duration well past the OLD 2h default (issue #235's
// own example of a real run exceeding it) still survives a RecoverExpired
// pass under the NEW default, so a still-live long run's item is never
// silently freed and double-claimed in the shipped config.
func TestDefaultClaimLeaseSurvivesARealisticLongRun(t *testing.T) {
	start := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	fakeNow := start
	ledger, err := localscheduler.OpenClaimLedger(
		filepath.Join(t.TempDir(), "claims.json"),
		localscheduler.WithLedgerClock(func() time.Time { return fakeNow }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim("issue-3", "long-run", "implementation", DefaultClaimLease); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}

	// Advance past the OLD 2h default (the realistic duration #235 itself
	// cites as reachable: implement -> reviewer gate -> make ci -> open-pr ->
	// a retried ci-poll) but still within the new, raised default.
	fakeNow = start.Add(3 * time.Hour)
	if released, err := ledger.RecoverExpired(fakeNow); err != nil || len(released) != 0 {
		t.Fatalf("a claim 3h into a run must survive under the raised default: released=%v err=%v", released, err)
	}
	if entry, held := ledger.Lookup("issue-3"); !held || entry.RunID != "long-run" {
		t.Fatalf("claim should still be held by long-run: %+v held=%v", entry, held)
	}

	// Sanity: the raised default is genuinely "comfortably above" — not a
	// fluke of this test's specific 3h probe point — pinned so a future
	// retune can't silently shrink it back toward the old, reachable value.
	if DefaultClaimLease < 4*time.Hour {
		t.Fatalf("DefaultClaimLease = %s, want comfortably above a realistic ci-poll-bearing run (>= 4h)", DefaultClaimLease)
	}
}
