package localscheduler

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/sharedclaim"
)

func TestSharedAdministrativeReleaseRevokesBeforeUncertainCleanup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	ledger, err := OpenClaimLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	store := &admissionStore{}
	key := ClaimKey{Gaggle: "g", Provider: "github", ExternalID: "42"}
	owner := sharedclaim.Owner{Instance: "instance", Run: "run", Token: "owner"}
	if ok, _, err := ledger.ClaimSharedScoped(t.Context(), store, "42", key, owner, "work", time.Minute); err != nil || !ok {
		t.Fatalf("acquire: %v %v", ok, err)
	}
	// Local revocation is a prerequisite even if the provider is reachable.
	ledger.path = t.TempDir()
	if err := ledger.ForceReleaseCoordinatedShared(t.Context(), store, "42", key, owner, "cli"); err == nil || store.writes != 1 {
		t.Fatalf("provider changed without durable revocation: %v writes=%d", err, store.writes)
	}
	entry, _ := ledger.LookupScoped(key)
	if entry.SharedRevoked || !entry.SharedDeadline.After(time.Now()) {
		t.Fatal("failed revocation changed in-memory authority")
	}
	ledger.path = path
	store.fail = true
	if err := ledger.ForceReleaseCoordinatedShared(t.Context(), store, "42", key, owner, "cli"); err == nil {
		t.Fatal("uncertain provider cleanup reported success")
	}
	ledger, err = OpenClaimLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	entry, held := ledger.LookupScoped(key)
	if !held || !entry.SharedRevoked || entry.SharedDeadline.After(time.Now()) || entry.SharedOwner != owner {
		t.Fatalf("restart lost revocation or cleanup custody: %+v", entry)
	}
	writes := store.writes
	if ok, _, err := ledger.ClaimSharedScoped(t.Context(), store, "42", key, owner, "work", time.Minute); err == nil || ok || store.writes != writes {
		t.Fatalf("revoked holder reacquired remote authority: %v %v", ok, err)
	}
	store.fail = false
	if err := ledger.ForceReleaseCoordinatedShared(t.Context(), store, "42", key, owner, "cli"); err != nil {
		t.Fatal(err)
	}
	if _, held := ledger.LookupScoped(key); held {
		t.Fatal("acknowledged administrative cleanup retained active custody")
	}
	if ok, _, err := ledger.ClaimScopedUntil(key, owner.Run, "work", time.Now().Add(time.Minute), owner); err == nil || ok {
		t.Fatal("released revocation was bypassed by direct local admission")
	}
	unmarked := entry
	unmarked.SharedRevoked = false
	unmarked.SharedDeadline = time.Time{}
	unmarked.SharedOwner = sharedclaim.Owner{}
	if ok, _, err := ledger.ReclaimAll([]ClaimEntry{unmarked}, owner.Run, "work", time.Minute); err == nil || ok {
		t.Fatal("batch reclaim erased durable revocation through an unmarked entry")
	}
	if ok, _, err := ledger.ClaimSharedScoped(t.Context(), store, "42", key, owner, "work", time.Minute); err == nil || ok || store.writes != writes {
		t.Fatal("released revocation was bypassed by remote reacquisition")
	}
	successor := sharedclaim.Owner{Instance: "instance", Run: "new-run", Token: "successor"}
	if ok, _, err := ledger.ClaimSharedScoped(t.Context(), store, "42", key, successor, "work", time.Minute); err != nil || !ok {
		t.Fatalf("revocation incorrectly blocked a new run: %v %v", ok, err)
	}
	if err := ledger.ForceReleaseCoordinatedShared(t.Context(), store, "42", key, owner, "cli"); err == nil || store.record.Owner != successor {
		t.Fatal("stale administrative request removed a successor")
	}
}
