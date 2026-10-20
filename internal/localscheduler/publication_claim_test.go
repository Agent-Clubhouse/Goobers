package localscheduler

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimrelease"
)

func TestPublicationClaimHasFixedCustodyIndependentOfRunLifecycle(t *testing.T) {
	now := time.Now().UTC()
	ledger, err := OpenClaimLedger(filepath.Join(t.TempDir(), "claims.json"), WithLedgerClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	key := ClaimKey{Gaggle: "test", Provider: "github", ExternalID: PublicationLockItemIDPrefix + "github/acme/repo"}
	if ok, _, err := ledger.ClaimScoped(key, "first", "review", time.Minute); err != nil || !ok {
		t.Fatalf("acquire: %t %v", ok, err)
	}
	before, _ := ledger.LookupScoped(key)
	now = now.Add(10 * time.Second)
	// Neither direct same-run acquisition, generic renewal, nor continuation
	// reclaim may extend or transfer custody for an uncertain provider action.
	if ok, _, err := ledger.ClaimScoped(key, "first", "review", time.Hour); err != nil || ok {
		t.Fatalf("same-run acquisition renewed custody: %t %v", ok, err)
	}
	if ok, err := ledger.RenewEntry(before, time.Hour); err != nil || ok {
		t.Fatalf("renewed custody: %t %v", ok, err)
	}
	if renewed, err := ledger.RenewRuns(map[string]bool{"first": true}, time.Hour); err != nil || len(renewed) != 0 {
		t.Fatalf("run renewal included custody: %+v %v", renewed, err)
	}
	if ok, _, err := ledger.ReclaimAll([]ClaimEntry{before}, "continuation", "review", time.Hour); err != nil || !ok {
		t.Fatalf("continuation work reclaim: %t %v", ok, err)
	}
	after, _ := ledger.LookupScoped(key)
	if after.RunID != before.RunID || !after.ExpiresAt.Equal(before.ExpiresAt) {
		t.Fatalf("lifecycle changed custody: before=%+v after=%+v", before, after)
	}
	if err := ledger.ReleaseScoped(key, "first", claimrelease.Completed.Reason()); err == nil {
		t.Fatal("unverified release ended custody")
	}
	if released, err := ledger.RecoverExpired(now); err != nil || len(released) != 0 {
		t.Fatalf("early reap: %+v %v", released, err)
	}
	now = before.ExpiresAt
	if released, err := ledger.RecoverExpired(now); err != nil || len(released) != 1 {
		t.Fatalf("bounded expiry did not end custody: %+v %v", released, err)
	}
	if ok, _, err := ledger.ClaimScoped(key, "second", "review", time.Minute); err != nil || !ok {
		t.Fatalf("acquire after expiry: %t %v", ok, err)
	}
	if err := ledger.ReleaseScoped(key, "second", claimrelease.Completed.Because(PublicationLockReleaseCode, "")); err != nil {
		t.Fatalf("verified release: %v", err)
	}
	if _, held := ledger.LookupScoped(key); held {
		t.Fatal("verified custody was not released")
	}
}
