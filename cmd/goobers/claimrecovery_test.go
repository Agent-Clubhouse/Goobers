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

	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim("issue-1", "crashed-run", "implementation", -time.Minute); err != nil || !ok {
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
