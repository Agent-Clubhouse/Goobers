package localscheduler

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/sharedclaim"
)

func TestSharedReservationSurvivesRestartWithoutAdmission(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	ledger, err := OpenClaimLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	key := ClaimKey{Gaggle: "g", Provider: "github", ExternalID: "42"}
	owner := sharedclaim.Owner{Instance: "instance", Run: "run", Token: "incarnation"}
	reserved, ok, _, err := ledger.BeginSharedAdmission(key, owner, "implement")
	if err != nil || !ok {
		t.Fatalf("reserve: %v %v", ok, err)
	}
	if reserved.ExpiresAt.After(time.Now()) {
		t.Fatal("reservation admitted execution")
	}
	ledger, err = OpenClaimLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if recovered, err := ledger.RecoverExpired(time.Now().Add(time.Hour)); err != nil || len(recovered) != 0 {
		t.Fatalf("recovery discarded reservation: %+v %v", recovered, err)
	}
	if ok, _, err := ledger.ClaimScoped(key, "other", "implement", time.Minute); err == nil && ok {
		t.Fatal("local claimant replaced pending custody")
	}
	if err := ledger.ReleaseScoped(key, owner.Run, claimrelease.Completed.Reason()); err == nil {
		t.Fatal("ordinary release discarded pending custody")
	}
	if _, ok, _, err := ledger.BeginSharedAdmission(key, sharedclaim.Owner{Instance: "instance", Run: "other", Token: "new"}, "implement"); err == nil && ok {
		t.Fatal("another incarnation replaced reservation")
	}
	retried, ok, _, err := ledger.BeginSharedAdmission(key, owner, "implement")
	if err != nil || !ok || !retried.ClaimedAt.Equal(reserved.ClaimedAt) {
		t.Fatalf("retry changed custody: %+v %v", retried, err)
	}
	if ok, _, err := ledger.FinishSharedAdmission(reserved, time.Now().Add(time.Minute)); err != nil || !ok {
		t.Fatalf("finalize: %v %v", ok, err)
	}
	current, _ := ledger.LookupScoped(key)
	if current.SharedAdmissionPending || !current.ExpiresAt.After(time.Now()) {
		t.Fatal("acknowledgment did not admit")
	}
	if ok, err := ledger.BeginSharedRelease(current); err != nil || !ok {
		t.Fatalf("release reservation: %v %v", ok, err)
	}
	ledger, err = OpenClaimLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _, err := ledger.BeginSharedAdmission(key, owner, "implement"); err == nil && ok {
		t.Fatal("renewal bypassed pending release")
	}
	if ok, err := ledger.FinishProviderRelease(current, claimrelease.Completed.Reason()); err != nil || !ok {
		t.Fatalf("release finalize: %v %v", ok, err)
	}
	if _, held := ledger.LookupScoped(key); held {
		t.Fatal("acknowledged release retained custody")
	}
}

func TestSharedReservationRevocationWinsOverRemoteAcknowledgment(t *testing.T) {
	ledger, err := OpenClaimLedger(filepath.Join(t.TempDir(), "claims.json"))
	if err != nil {
		t.Fatal(err)
	}
	key := ClaimKey{Gaggle: "g", Provider: "github", ExternalID: "42"}
	owner := sharedclaim.Owner{Instance: "instance", Run: "run", Token: "incarnation"}
	pending, ok, _, err := ledger.BeginSharedAdmission(key, owner, "implement")
	if err != nil || !ok {
		t.Fatalf("reserve: %v %v", ok, err)
	}
	if ok, err := ledger.RevokeSharedExecution(key, owner, "operator"); err != nil || !ok {
		t.Fatalf("revoke: %v %v", ok, err)
	}
	if ok, _, err := ledger.FinishSharedAdmission(pending, time.Now().Add(time.Hour)); err == nil || ok {
		t.Fatal("late acknowledgment restored revoked execution")
	}
	if ok, err := ledger.BeginSharedRelease(pending); err != nil || !ok {
		t.Fatalf("revoked cleanup reserve: %v %v", ok, err)
	}
	if ok, err := ledger.FinishProviderRelease(pending, claimrelease.ForceReleased.Reason().By("operator")); err != nil || !ok {
		t.Fatalf("revoked cleanup: %v %v", ok, err)
	}
	if _, ok, _, err := ledger.BeginSharedAdmission(key, owner, "implement"); err == nil || ok {
		t.Fatal("revoked history allowed readmission")
	}
}
