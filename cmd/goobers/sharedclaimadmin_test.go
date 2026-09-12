package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/sharedclaim"
)

func TestAdministrativeSharedReleaseRevokesEvenWithoutCredentials(t *testing.T) {
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(t.TempDir(), "claims.json"))
	if err != nil {
		t.Fatal(err)
	}
	key := localscheduler.ClaimKey{Gaggle: "g", Provider: "github", ExternalID: "42"}
	owner := sharedclaim.Owner{Instance: "instance", Run: "run", Token: "token"}
	if ok, _, err := ledger.ClaimScopedUntil(key, owner.Run, "work", time.Now().Add(time.Minute), owner); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	entry, _ := ledger.LookupScoped(key)
	if err := forceReleaseClaim(t.Context(), ledger, nil, entry, "cli"); err == nil {
		t.Fatal("missing provider cleanup was reported as successful release")
	}
	current, held := ledger.LookupScoped(key)
	if !held || !current.SharedRevoked || current.SharedDeadline.After(time.Now()) || current.SharedOwner != owner {
		t.Fatalf("missing credentials prevented revocation or lost custody: %+v", current)
	}
}
