package localscheduler

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/sharedclaim"
)

func TestSharedReleaseRequiresExactPersistedIncarnation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	ledger, err := OpenClaimLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	key := ClaimKey{Gaggle: "gaggle", Provider: "github", ExternalID: "42"}
	owner := sharedclaim.Owner{Instance: "instance", Run: "run", Token: "current"}
	if ok, _, err := ledger.ClaimScopedUntil(key, owner.Run, "implement", time.Now().Add(time.Minute), owner); err != nil || !ok {
		t.Fatalf("claim: %t %v", ok, err)
	}
	ledger, err = OpenClaimLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := ledger.LookupScoped(key)
	if err := ledger.ReleaseEntry(entry, owner.Run); err == nil {
		t.Fatal("local-only release discarded shared reconciliation owner")
	}
	if err := ledger.ForceReleaseEntry(entry, "cli"); err == nil {
		t.Fatal("local-only administrative release discarded remote ownership")
	}
	stale := owner
	stale.Token = "previous"
	if err := ledger.ReleaseSharedScoped(key, stale); !errors.Is(err, sharedclaim.ErrNotOwner) {
		t.Fatalf("stale release: %v", err)
	}
	if got, found := ledger.LookupScoped(key); !found || got.SharedOwner != owner {
		t.Fatal("refused release changed live ownership")
	}
	if err := ledger.ReleaseSharedScoped(key, owner); err != nil {
		t.Fatal(err)
	}
	if _, found := ledger.LookupScoped(key); found {
		t.Fatal("acknowledged release left claim active")
	}
	if err := ledger.ReleaseSharedScoped(key, owner); err != nil {
		t.Fatalf("release retry: %v", err)
	}
}
