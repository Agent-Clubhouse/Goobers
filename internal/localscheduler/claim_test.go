package localscheduler

import (
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClaimAndRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	l, err := OpenClaimLedger(path)
	if err != nil {
		t.Fatal(err)
	}

	ok, holder, err := l.Claim("issue-8", "run-a", "curate", time.Minute)
	if err != nil || !ok || holder != "run-a" {
		t.Fatalf("first claim should succeed: ok=%v holder=%s err=%v", ok, holder, err)
	}

	ok, holder, err = l.Claim("issue-8", "run-b", "curate", time.Minute)
	if err != nil || ok || holder != "run-a" {
		t.Fatalf("second claimant should be refused, holder=run-a: ok=%v holder=%s err=%v", ok, holder, err)
	}

	// Idempotent re-claim by the same run succeeds (retried backlog-query stage).
	ok, holder, err = l.Claim("issue-8", "run-a", "curate", time.Minute)
	if err != nil || !ok || holder != "run-a" {
		t.Fatalf("re-claim by the same run should succeed: ok=%v holder=%s err=%v", ok, holder, err)
	}

	if err := l.Release("issue-8", "run-a"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, held := l.Lookup("issue-8"); held {
		t.Fatal("item should be unheld after release")
	}

	ok, holder, err = l.Claim("issue-8", "run-c", "curate", time.Minute)
	if err != nil || !ok || holder != "run-c" {
		t.Fatalf("claim after release should succeed: ok=%v holder=%s err=%v", ok, holder, err)
	}
}

func TestReleaseIsIdempotentAndOwnerScoped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	l, err := OpenClaimLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Claim("item", "run-a", "wf", time.Minute); err != nil {
		t.Fatal(err)
	}

	// A release from a non-owner is a no-op — must not release someone else's claim.
	if err := l.Release("item", "run-b"); err != nil {
		t.Fatal(err)
	}
	if e, held := l.Lookup("item"); !held || e.RunID != "run-a" {
		t.Fatalf("non-owner release must not affect the claim: %+v held=%v", e, held)
	}

	if err := l.Release("item", "run-a"); err != nil {
		t.Fatal(err)
	}
	// Double release is a no-op, not an error.
	if err := l.Release("item", "run-a"); err != nil {
		t.Fatalf("double release should be a no-op: %v", err)
	}
}

// TestCrashRecoveryReleasesExpiredLeaseExactlyOnce is the headline acceptance
// criterion: a run claims an item, "crashes" (never releases), and after the
// lease expires, recovery releases it and the item is claimable again exactly
// once — a second recovery pass or a second claimant racing in does not
// double-grant it.
func TestCrashRecoveryReleasesExpiredLeaseExactlyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	l, err := OpenClaimLedger(path, WithLedgerClock(clock))
	if err != nil {
		t.Fatal(err)
	}

	ok, _, err := l.Claim("issue-9", "run-crashed", "curate", 30*time.Second)
	if err != nil || !ok {
		t.Fatalf("initial claim: ok=%v err=%v", ok, err)
	}
	// "Crash": no Release call. Advance time past the lease expiry.
	now = now.Add(time.Minute)

	released, err := l.RecoverExpired(now)
	if err != nil {
		t.Fatalf("RecoverExpired: %v", err)
	}
	if len(released) != 1 || released[0].RunID != "run-crashed" || released[0].ItemID != "issue-9" {
		t.Fatalf("expected to release the crashed run's lease exactly once: %+v", released)
	}

	// A second recovery pass finds nothing left to release.
	released2, err := l.RecoverExpired(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(released2) != 0 {
		t.Fatalf("second recovery pass should be a no-op: %+v", released2)
	}

	// The item is claimable again — exactly once: the first claimant after
	// recovery wins, a second is refused.
	ok, holder, err := l.Claim("issue-9", "run-retry-a", "curate", time.Minute)
	if err != nil || !ok || holder != "run-retry-a" {
		t.Fatalf("item should be claimable again after recovery: ok=%v holder=%s err=%v", ok, holder, err)
	}
	ok, holder, err = l.Claim("issue-9", "run-retry-b", "curate", time.Minute)
	if err != nil || ok || holder != "run-retry-a" {
		t.Fatalf("only one claimant should win the reclaim: ok=%v holder=%s err=%v", ok, holder, err)
	}
}

// TestClaimSurvivesReopen proves the ledger is durable: a claim made before
// "closing" (simulated by simply opening a fresh ClaimLedger over the same
// path — there's no separate Close, the file is the persisted state) is still
// held after reopening.
func TestClaimSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	l1, err := OpenClaimLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := l1.Claim("issue-1", "run-a", "wf", time.Hour); err != nil {
		t.Fatal(err)
	}

	l2, err := OpenClaimLedger(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	entry, held := l2.Lookup("issue-1")
	if !held || entry.RunID != "run-a" {
		t.Fatalf("claim should survive reopen: entry=%+v held=%v", entry, held)
	}
}

// TestClaimPersistFailureLeavesItemClaimable proves the ledger rolls back its
// in-memory mutation when the durable write fails: a claim whose persist errors
// must report ok=false, leave no phantom hold, and leave the item claimable by
// another run — in-memory and durable state must never diverge.
func TestClaimPersistFailureLeavesItemClaimable(t *testing.T) {
	dir := t.TempDir()
	goodPath := filepath.Join(dir, "claims.json")
	l, err := OpenClaimLedger(goodPath)
	if err != nil {
		t.Fatal(err)
	}

	// Force persist to fail by pointing the ledger at a path whose parent dir
	// does not exist, so the atomic write cannot land.
	l.path = filepath.Join(dir, "missing-subdir", "claims.json")

	ok, _, err := l.Claim("issue-42", "run-a", "curate", time.Minute)
	if err == nil {
		t.Fatal("expected a persist error, got nil")
	}
	if ok {
		t.Fatal("Claim must report ok=false when the claim did not persist")
	}

	// Rollback: the failed claim must not leave a lingering in-memory hold.
	if e, held := l.Lookup("issue-42"); held {
		t.Fatalf("failed-persist claim must be rolled back, but item is held by %q", e.RunID)
	}

	// Claimable again: restore a writable path and let a DIFFERENT run claim it.
	// It must win — not be refused by run-a's rolled-back ghost claim.
	l.path = goodPath
	ok, holder, err := l.Claim("issue-42", "run-b", "curate", time.Minute)
	if err != nil || !ok || holder != "run-b" {
		t.Fatalf("item must be claimable after a persist failure: ok=%v holder=%s err=%v", ok, holder, err)
	}
}

// TestClaimPersistFailurePreservesPriorOwner proves rollback restores the prior
// owner (not just "unheld"): when a run renews its own live lease and the renewal
// persist fails, the run's existing claim must remain intact in memory.
func TestClaimPersistFailurePreservesPriorOwner(t *testing.T) {
	dir := t.TempDir()
	goodPath := filepath.Join(dir, "claims.json")
	l, err := OpenClaimLedger(goodPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Claim("issue-7", "run-a", "curate", time.Hour); err != nil {
		t.Fatal(err)
	}

	// A same-run renewal whose persist fails must not drop the existing claim.
	l.path = filepath.Join(dir, "missing-subdir", "claims.json")
	if _, _, err := l.Claim("issue-7", "run-a", "curate", time.Hour); err == nil {
		t.Fatal("expected a persist error on the failed renewal")
	}
	e, held := l.Lookup("issue-7")
	if !held || e.RunID != "run-a" {
		t.Fatalf("a failed renewal must preserve the prior owner's claim: %+v held=%v", e, held)
	}

	// And a different run is still refused — the claim genuinely survived.
	l.path = goodPath
	ok, holder, err := l.Claim("issue-7", "run-b", "curate", time.Minute)
	if err != nil || ok || holder != "run-a" {
		t.Fatalf("prior owner should still hold the claim: ok=%v holder=%s err=%v", ok, holder, err)
	}
}

// TestClaimConcurrentRace is the "two schedulers/runs don't double-start the
// same work" property under real concurrency: N goroutines race to claim the
// same item; exactly one must win.
func TestClaimConcurrentRace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	l, err := OpenClaimLedger(path)
	if err != nil {
		t.Fatal(err)
	}

	const workers = 100
	var wins int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			runID := fmt.Sprintf("run-%d", n)
			if ok, _, err := l.Claim("contested-item", runID, "wf", time.Minute); err == nil && ok {
				atomic.AddInt64(&wins, 1)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if wins != 1 {
		t.Fatalf("expected exactly 1 winner, got %d", wins)
	}
}
