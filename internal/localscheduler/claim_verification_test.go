package localscheduler

import (
	"path/filepath"
	"testing"
	"time"
)

func TestClaimVerificationIsScopedAndLeaseBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	ledger, err := OpenClaimLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	key := ClaimKey{Gaggle: "team", Provider: "github", ExternalID: "7"}
	if ok, _, err := ledger.ClaimScoped(key, "run-a", "implement", time.Hour); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	entry, _ := ledger.LookupScoped(key)
	observation := ClaimVerification{State: "verified", ObservedAt: time.Now(), ProviderRunID: "run-a"}
	if ok, err := ledger.RecordClaimVerification(entry, observation); err != nil || !ok {
		t.Fatalf("record: %v %v", ok, err)
	}
	reopened, err := OpenClaimLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := reopened.LookupScoped(key)
	if got.Verification.State != "verified" || !got.ExpiresAt.Equal(entry.ExpiresAt) {
		t.Fatalf("observation missing or lease extended: %+v", got)
	}
	if err := ledger.ReleaseScoped(key, "run-a"); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.ClaimScoped(key, "run-b", "implement", time.Hour); err != nil || !ok {
		t.Fatalf("replacement: %v %v", ok, err)
	}
	if ok, err := ledger.RecordClaimVerification(entry, observation); err != nil || ok {
		t.Fatalf("stale observation accepted: %v %v", ok, err)
	}
	got, _ = ledger.LookupScoped(key)
	if got.Verification.State != "" {
		t.Fatalf("replacement inherited verification: %+v", got)
	}
}

func TestClaimVerificationRejectsContradictoryAndStaleObservations(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	ledger, err := OpenClaimLedger(filepath.Join(t.TempDir(), "claims.json"), WithLedgerClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	key := ClaimKey{Gaggle: "team", Provider: "github", ExternalID: "7"}
	if ok, _, err := ledger.ClaimScoped(key, "owner", "implement", time.Hour); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	entry, _ := ledger.LookupScoped(key)
	now = now.Add(time.Minute)
	for _, observation := range []ClaimVerification{
		{State: "verified", ObservedAt: now, ProviderRunID: "other"},
		{State: "ownership-mismatch", ObservedAt: now, ProviderRunID: "owner"},
		{State: "ownership-mismatch", ObservedAt: now},
		{State: "missing", ObservedAt: now, ProviderRunID: "owner"},
		{State: "unavailable", ObservedAt: now, ProviderRunID: "owner"},
		{State: "verified", ObservedAt: now.Add(time.Second), ProviderRunID: "owner"},
	} {
		if ok, err := ledger.RecordClaimVerification(entry, observation); err == nil || ok {
			t.Fatalf("invalid observation accepted: %+v (%v, %v)", observation, ok, err)
		}
	}
	observation := ClaimVerification{State: "verified", ObservedAt: now, ProviderRunID: "owner"}
	if ok, err := ledger.RecordClaimVerification(entry, observation); err != nil || !ok {
		t.Fatalf("valid observation: %v %v", ok, err)
	}
	if history := ledger.HistorySnapshot(); len(history) != 1 || history[0].Verification != observation {
		t.Fatalf("history missed verification: %+v", history)
	}
	for _, at := range []time.Time{now, now.Add(-time.Second)} {
		if ok, err := ledger.RecordClaimVerification(entry, ClaimVerification{State: "missing", ObservedAt: at}); err != nil || ok {
			t.Fatalf("older/equal observation replaced newer one: %v %v", ok, err)
		}
	}
	now = now.Add(2 * time.Hour)
	if ok, err := ledger.RecordClaimVerification(entry, ClaimVerification{State: "missing", ObservedAt: now}); err != nil || ok {
		t.Fatalf("expired lease observed as current: %v %v", ok, err)
	}
}

func TestClaimVerificationSurvivesOnlyLiveRenewalAndRollsBackOnStoreFailure(t *testing.T) {
	now := time.Now().UTC()
	path := filepath.Join(t.TempDir(), "claims.json")
	ledger, err := OpenClaimLedger(path, WithLedgerClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	key := ClaimKey{Gaggle: "g", Provider: "github", ExternalID: "7"}
	if ok, _, err := ledger.ClaimScoped(key, "owner", "w", time.Hour); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	entry, _ := ledger.LookupScoped(key)
	now = now.Add(time.Second)
	observation := ClaimVerification{State: "verified", ObservedAt: now, ProviderRunID: "owner"}
	if ok, err := ledger.RecordClaimVerification(entry, observation); err != nil || !ok {
		t.Fatalf("record: %v %v", ok, err)
	}
	now = now.Add(time.Second)
	if ok, err := ledger.RenewEntry(entry, time.Hour); err != nil || !ok {
		t.Fatalf("renew: %v %v", ok, err)
	}
	current, _ := ledger.LookupScoped(key)
	if current.Verification != observation {
		t.Fatalf("renewal erased or freshened observation: %+v", current)
	}
	// Existing file used as a parent forces persistence failure on every OS.
	ledger.path = filepath.Join(path, "impossible-child")
	now = now.Add(time.Second)
	if ok, err := ledger.RecordClaimVerification(current, ClaimVerification{State: "missing", ObservedAt: now}); err == nil || ok {
		t.Fatalf("store failure accepted: %v %v", ok, err)
	}
	got, _ := ledger.LookupScoped(key)
	if got.Verification != observation || ledger.HistorySnapshot()[0].Verification != observation {
		t.Fatal("failed write changed memory or history")
	}
	ledger.path = path
	reopened, err := OpenClaimLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = reopened.LookupScoped(key)
	if got.Verification != observation {
		t.Fatal("failed write changed durable state")
	}
	now = now.Add(2 * time.Hour)
	if ok, _, err := ledger.ClaimScoped(key, "owner", "w", time.Hour); err != nil || !ok {
		t.Fatalf("reacquire: %v %v", ok, err)
	}
	got, _ = ledger.LookupScoped(key)
	if got.Verification.State != "" {
		t.Fatalf("expired lease verification inherited: %+v", got)
	}
}
