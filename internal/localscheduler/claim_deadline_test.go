package localscheduler

import (
	"path/filepath"
	"testing"
	"time"
)

func TestClaimScopedUntilPersistsExactDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	now := time.Now().UTC()
	ledger, err := OpenClaimLedger(path, WithLedgerClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	key := ClaimKey{Gaggle: "gaggle", Provider: "github", ExternalID: "42"}
	deadline := now.Add(time.Minute)
	// Time spent reaching the local ledger must not extend the remote lease.
	now = now.Add(20 * time.Second)
	if ok, _, err := ledger.ClaimScopedUntil(key, "run", "implement", deadline); err != nil || !ok {
		t.Fatalf("claim: %v, %v", ok, err)
	}
	reopened, err := OpenClaimLedger(path, WithLedgerClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	entry, found := reopened.LookupScoped(key)
	if !found || !entry.ExpiresAt.Equal(deadline) {
		t.Fatalf("persisted deadline changed: %+v", entry)
	}
	if !entry.SharedDeadline.Equal(deadline) {
		t.Fatal("restart lost shared admission marker")
	}
	if ok, err := reopened.RenewEntry(entry, time.Hour); err == nil || ok {
		t.Fatalf("local-only renewal extended shared admission: %v, %v", ok, err)
	}
	if ok, _, err := reopened.ReclaimAll([]ClaimEntry{entry}, "resumed", "implement", time.Hour); err == nil || ok {
		t.Fatalf("local-only reclaim copied shared admission: %v, %v", ok, err)
	}
	// Even a caller omitting the marker cannot replace the durable admission
	// through the legacy batch path.
	unmarked := entry
	unmarked.SharedDeadline = time.Time{}
	if ok, _, err := reopened.ReclaimAll([]ClaimEntry{unmarked}, "run", "implement", time.Hour); err == nil || ok {
		t.Fatalf("local-only reclaim discarded shared admission: %v, %v", ok, err)
	}
	for _, expired := range []time.Time{{}, now, now.Add(-time.Second)} {
		if ok, _, err := ledger.ClaimScopedUntil(key, "run", "implement", expired); err == nil || ok {
			t.Fatalf("expired renewal admitted: %v, %v", ok, err)
		}
	}
	entry, found = ledger.LookupScoped(key)
	if !found || !entry.ExpiresAt.Equal(deadline) {
		t.Fatal("refused renewal mutated existing claim")
	}
	now = deadline
	if ok, _, err := ledger.ClaimScopedUntil(key, "successor", "implement", now.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("expired claim prevents successor admission: %v, %v", ok, err)
	}
}
