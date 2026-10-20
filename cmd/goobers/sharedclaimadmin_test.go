package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/sharedclaim"
)

func TestAdministrativeSharedReleaseRevokesEvenWithoutCredentials(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	path := filepath.Join(layout.SchedulerDir(), claimLedgerFileName)
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	ledger, err := localscheduler.OpenClaimLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	key := localscheduler.ClaimKey{Gaggle: "g", Provider: "github", ExternalID: "42"}
	owner := sharedclaim.Owner{Instance: "instance", Run: "run", Token: "token"}
	if ok, _, err := ledger.ClaimScopedUntil(key, owner.Run, "work", time.Now().Add(time.Minute), owner); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	entry, _ := ledger.LookupScoped(key)
	if err := forceReleaseClaim(t.Context(), layout, nil, nil, entry, "cli"); err == nil {
		t.Fatal("missing provider cleanup was reported as successful release")
	}
	ledger, err = localscheduler.OpenClaimLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	current, held := ledger.LookupScoped(key)
	if !held || !current.SharedRevoked || current.SharedDeadline.After(time.Now()) || current.SharedOwner != owner {
		t.Fatalf("missing credentials prevented revocation or lost custody: %+v", current)
	}
}

func TestSharedAdminRejectsPublicationOverrideBeforeRevocation(t *testing.T) {
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(t.TempDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	key := localscheduler.ClaimKey{Gaggle: "g", Provider: "github", ExternalID: "42"}
	owner := sharedclaim.Owner{Instance: "instance", Run: "run", Token: "token"}
	if ok, _, err := ledger.ClaimScopedUntil(key, owner.Run, "work", time.Now().Add(time.Minute), owner); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	entry, _ := ledger.LookupScoped(key)
	request := claimAdminRequest{Actor: claimAdminActorCLI, Operator: "operator", PublicationReason: "verified remotely"}
	if cleanup, err := releaseAdminClaim(ledger, entry, request); err == nil || cleanup {
		t.Fatalf("publication override accepted an ordinary shared key: %v %v", cleanup, err)
	}
	current, held := ledger.LookupScoped(key)
	if !held || current.SharedRevoked || !current.ExpiresAt.Equal(entry.ExpiresAt) {
		t.Fatal("invalid override changed shared authority")
	}
	request.PublicationReason = ""
	if cleanup, err := releaseAdminClaim(ledger, entry, request); err != nil || !cleanup {
		t.Fatalf("shared revocation did not request provider cleanup: %v %v", cleanup, err)
	}
	current, held = ledger.LookupScoped(key)
	if !held || !current.SharedRevoked {
		t.Fatal("shared cleanup did not retain revoked custody")
	}
}
