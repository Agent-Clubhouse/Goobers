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

func TestClaimsAreIndependentAcrossGaggles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	ledger, err := OpenClaimLedger(path)
	if err != nil {
		t.Fatal(err)
	}

	alpha := ClaimKey{Gaggle: "alpha", Provider: "github", ExternalID: "159"}
	beta := ClaimKey{Gaggle: "beta", Provider: "github", ExternalID: "159"}

	if ok, _, err := ledger.ClaimScoped(alpha, "run-alpha", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("claim alpha: ok=%v err=%v", ok, err)
	}
	if ok, _, err := ledger.ClaimScoped(beta, "run-beta", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("claim beta: ok=%v err=%v", ok, err)
	}
	if ok, holder, err := ledger.ClaimScoped(alpha, "run-other", "implementation", time.Hour); err != nil || ok || holder != "run-alpha" {
		t.Fatalf("duplicate alpha claim: ok=%v holder=%q err=%v", ok, holder, err)
	}

	reopened, err := OpenClaimLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if entry, ok := reopened.LookupScoped(alpha); !ok || entry.RunID != "run-alpha" {
		t.Fatalf("alpha lookup = %+v, %v", entry, ok)
	}
	if entry, ok := reopened.LookupScoped(beta); !ok || entry.RunID != "run-beta" {
		t.Fatalf("beta lookup = %+v, %v", entry, ok)
	}
	if err := reopened.ReleaseScoped(alpha, "run-alpha"); err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.LookupScoped(alpha); ok {
		t.Fatal("alpha claim still held after release")
	}
	if entry, ok := reopened.LookupScoped(beta); !ok || entry.RunID != "run-beta" {
		t.Fatalf("releasing alpha changed beta claim: %+v, %v", entry, ok)
	}
}

func TestMigrateLegacyClaimNamespace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	ledger, err := OpenClaimLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim("159", "legacy-run", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("legacy claim: ok=%v err=%v", ok, err)
	}
	if err := ledger.MigrateLegacyNamespace("alpha", "github"); err != nil {
		t.Fatal(err)
	}
	key := ClaimKey{Gaggle: "alpha", Provider: "github", ExternalID: "159"}
	if entry, ok := ledger.LookupScoped(key); !ok || entry.RunID != "legacy-run" {
		t.Fatalf("migrated claim = %+v, %v", entry, ok)
	}
	if _, ok := ledger.Lookup("159"); ok {
		t.Fatal("legacy item-only key still exists")
	}
}

func TestMigrateLegacyClaimsUsesPerRunOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	ledger, err := OpenClaimLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, claim := range []struct {
		itemID string
		runID  string
	}{
		{itemID: "159", runID: "run-alpha"},
		{itemID: "27", runID: "run-beta"},
	} {
		if ok, _, err := ledger.Claim(claim.itemID, claim.runID, "implementation", time.Hour); err != nil || !ok {
			t.Fatalf("legacy claim %s: ok=%v err=%v", claim.itemID, ok, err)
		}
	}

	namespaces := map[string]ClaimNamespace{
		"run-alpha": {Gaggle: "alpha", Provider: "github"},
		"run-beta":  {Gaggle: "beta", Provider: "ado"},
	}
	if err := ledger.MigrateLegacyClaims(func(entry ClaimEntry) (ClaimNamespace, error) {
		namespace, ok := namespaces[entry.RunID]
		if !ok {
			return ClaimNamespace{}, fmt.Errorf("unknown run %s", entry.RunID)
		}
		return namespace, nil
	}); err != nil {
		t.Fatal(err)
	}

	for _, key := range []ClaimKey{
		{Gaggle: "alpha", Provider: "github", ExternalID: "159"},
		{Gaggle: "beta", Provider: "ado", ExternalID: "27"},
	} {
		if entry, ok := ledger.LookupScoped(key); !ok || entry.Gaggle != key.Gaggle || entry.Provider != key.Provider {
			t.Fatalf("migrated claim %v = %+v, %v", key, entry, ok)
		}
		if _, ok := ledger.Lookup(key.ExternalID); ok {
			t.Fatalf("legacy item-only key %q still exists", key.ExternalID)
		}
	}
}

// TestClaimRejectsNonPositiveLeaseDuration is issue #235 edge 1: a
// non-positive leaseDuration computes ExpiresAt <= ClaimedAt, so the entry
// is expired() at the moment it's written — the very check the exclusivity
// guard relies on — which would let a second claimant win immediately
// instead of being refused. Claim must fail closed before ever touching the
// ledger, on both an unheld item and one already held live.
func TestClaimRejectsNonPositiveLeaseDuration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	l, err := OpenClaimLedger(path)
	if err != nil {
		t.Fatal(err)
	}

	for _, d := range []time.Duration{0, -time.Minute, -time.Hour} {
		ok, holder, err := l.Claim("issue-9", "run-a", "curate", d)
		if err == nil || ok || holder != "" {
			t.Fatalf("leaseDuration=%s: Claim should fail closed, got ok=%v holder=%q err=%v", d, ok, holder, err)
		}
		if _, held := l.Lookup("issue-9"); held {
			t.Fatalf("leaseDuration=%s: a rejected claim must not touch the ledger", d)
		}
	}

	// A non-positive duration on a live claim (held by a DIFFERENT run) must
	// be refused too, not silently succeed via the exclusivity check's own
	// early-return — validation runs before that check.
	if ok, _, err := l.Claim("issue-9", "run-a", "curate", time.Hour); err != nil || !ok {
		t.Fatalf("seed a live claim: ok=%v err=%v", ok, err)
	}
	if ok, holder, err := l.Claim("issue-9", "run-b", "curate", 0); err == nil || ok {
		t.Fatalf("leaseDuration=0 against a live claim should fail closed, not silently 'succeed' by hitting the exclusivity branch: ok=%v holder=%q err=%v", ok, holder, err)
	}
}

// TestForRun proves the #131/#132 lookup a downstream stage (issue-close-out)
// uses to recover which item its own run claimed, several stages after
// backlog-query and in a different worktree/process.
func TestForRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	l, err := OpenClaimLedger(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := l.ForRun("run-a"); ok {
		t.Fatal("no claim yet: ForRun should report false")
	}

	if ok, _, err := l.Claim("issue-8", "run-a", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}

	entry, ok := l.ForRun("run-a")
	if !ok || entry.ItemID != "issue-8" || entry.RunID != "run-a" {
		t.Fatalf("ForRun(run-a) = %+v, %v, want issue-8 held by run-a", entry, ok)
	}
	if _, ok := l.ForRun("run-b"); ok {
		t.Fatal("a different run should not resolve another run's claim")
	}

	if err := l.Release("issue-8", "run-a"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, ok := l.ForRun("run-a"); ok {
		t.Fatal("released claim should no longer resolve via ForRun")
	}
}

func TestForRunAll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	l, err := OpenClaimLedger(path)
	if err != nil {
		t.Fatal(err)
	}

	if entries := l.ForRunAll("run-a"); len(entries) != 0 {
		t.Fatalf("no claims yet: ForRunAll returned %+v", entries)
	}
	for _, itemID := range []string{"issue-9", "issue-8"} {
		if ok, _, err := l.Claim(itemID, "run-a", "backlog-curation", time.Hour); err != nil || !ok {
			t.Fatalf("Claim(%s): ok=%v err=%v", itemID, ok, err)
		}
	}
	if ok, _, err := l.Claim("issue-10", "run-b", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("Claim(issue-10): ok=%v err=%v", ok, err)
	}

	entries := l.ForRunAll("run-a")
	if len(entries) != 2 || entries[0].ItemID != "issue-8" || entries[1].ItemID != "issue-9" {
		t.Fatalf("ForRunAll(run-a) = %+v, want issue-8 and issue-9 in item ID order", entries)
	}
	if entries := l.ForRunAll("run-b"); len(entries) != 1 || entries[0].ItemID != "issue-10" {
		t.Fatalf("ForRunAll(run-b) = %+v, want only issue-10", entries)
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

// TestClaimPersistFailurePreservesPriorOwner proves rollback restores the
// prior owner's ENTRY, not just its RunID (issue #177, hardening a test that
// previously passed with or without the hadPrev rollback): in the same-run
// renewal case, the phantom entry left on a failed persist is still owned by
// run-a either way, so an ownership-only assertion can't distinguish "rolled
// back" from "rollback is a no-op." Renewing with a DIFFERENT leaseDuration
// than the original claim, and asserting ExpiresAt reverts to the ORIGINAL
// value (not the failed renewal's would-be new one), is the one thing the
// hadPrev branch actually changes — so this fails against pre-#177 code that
// skips the restore. A fixed clock (WithLedgerClock) makes ExpiresAt an
// exact, comparable value instead of a wall-clock-dependent one.
func TestClaimPersistFailurePreservesPriorOwner(t *testing.T) {
	dir := t.TempDir()
	goodPath := filepath.Join(dir, "claims.json")
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	l, err := OpenClaimLedger(goodPath, WithLedgerClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Claim("issue-7", "run-a", "curate", time.Hour); err != nil {
		t.Fatal(err)
	}
	originalExpiry := now.Add(time.Hour)

	// A same-run renewal, with a DIFFERENT leaseDuration than the original
	// claim, whose persist fails must not adopt the new (would-be) expiry —
	// it must roll back to the original entry exactly, ExpiresAt included.
	l.path = filepath.Join(dir, "missing-subdir", "claims.json")
	if _, _, err := l.Claim("issue-7", "run-a", "curate", 2*time.Hour); err == nil {
		t.Fatal("expected a persist error on the failed renewal")
	}
	e, held := l.Lookup("issue-7")
	if !held || e.RunID != "run-a" {
		t.Fatalf("a failed renewal must preserve the prior owner's claim: %+v held=%v", e, held)
	}
	if !e.ExpiresAt.Equal(originalExpiry) {
		t.Fatalf("a failed renewal must restore the ORIGINAL ExpiresAt (rollback), got %s want %s", e.ExpiresAt, originalExpiry)
	}

	// And a different run is still refused — the claim genuinely survived.
	l.path = goodPath
	ok, holder, err := l.Claim("issue-7", "run-b", "curate", time.Minute)
	if err != nil || ok || holder != "run-a" {
		t.Fatalf("prior owner should still hold the claim: ok=%v holder=%s err=%v", ok, holder, err)
	}
}

// TestReleasePersistFailureLeavesClaimHeld proves Release rolls back its
// in-memory deletion when the durable write fails — the same rollback
// discipline TestClaimPersistFailureLeavesItemClaimable proves for Claim
// (issue #138: Release had the mutate-before-persist bug PR #99 fixed only
// for Claim). Without rollback, a failed-persist Release would leave the
// item claimable in memory while the durable ledger (and a crash-recovery
// reread) still shows it held — a different run could then win a claim the
// crashed process still believes it owns.
func TestReleasePersistFailureLeavesClaimHeld(t *testing.T) {
	dir := t.TempDir()
	goodPath := filepath.Join(dir, "claims.json")
	l, err := OpenClaimLedger(goodPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Claim("issue-51", "run-a", "curate", time.Hour); err != nil {
		t.Fatal(err)
	}

	l.path = filepath.Join(dir, "missing-subdir", "claims.json")
	if err := l.Release("issue-51", "run-a"); err == nil {
		t.Fatal("expected a persist error on the failed release")
	}

	e, held := l.Lookup("issue-51")
	if !held || e.RunID != "run-a" {
		t.Fatalf("a failed-persist release must leave the claim intact: %+v held=%v", e, held)
	}

	// And a different run is still refused — the claim genuinely survived,
	// not just present in a struct nobody checks.
	l.path = goodPath
	ok, holder, err := l.Claim("issue-51", "run-b", "curate", time.Minute)
	if err != nil || ok || holder != "run-a" {
		t.Fatalf("prior owner should still hold the claim: ok=%v holder=%s err=%v", ok, holder, err)
	}
}

// TestRecoverExpiredPersistFailureRestoresEntries proves RecoverExpired rolls
// back every deletion from a pass whose persist fails, restoring both the
// in-memory entries and reporting the failure (not a partial/discarded
// (nil, err) that loses track of what needed releasing) — issue #138. A
// caller retrying on its next periodic pass must see the same expired leases
// again, not a ledger that silently lost them.
func TestRecoverExpiredPersistFailureRestoresEntries(t *testing.T) {
	dir := t.TempDir()
	goodPath := filepath.Join(dir, "claims.json")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	l, err := OpenClaimLedger(goodPath, WithLedgerClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Claim("issue-52", "run-crashed", "curate", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)

	l.path = filepath.Join(dir, "missing-subdir", "claims.json")
	released, err := l.RecoverExpired(now)
	if err == nil {
		t.Fatal("expected a persist error on the failed recovery pass")
	}
	if released != nil {
		t.Fatalf("a failed pass must not report any released entries: %+v", released)
	}

	e, held := l.Lookup("issue-52")
	if !held || e.RunID != "run-crashed" {
		t.Fatalf("a failed-persist recovery must restore the expired entry: %+v held=%v", e, held)
	}

	// Retry with a writable path: the same lease must still be there to release.
	l.path = goodPath
	released, err = l.RecoverExpired(now)
	if err != nil {
		t.Fatalf("retry RecoverExpired: %v", err)
	}
	if len(released) != 1 || released[0].ItemID != "issue-52" {
		t.Fatalf("retry should release the same lease the failed pass restored: %+v", released)
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
