package localscheduler

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/sharedclaim"
)

type admissionStore struct {
	record   sharedclaim.Record
	revision string
	writes   int
	fail     bool
}

func (s *admissionStore) Read(context.Context, string) (sharedclaim.Observation, error) {
	return sharedclaim.Observation{Record: s.record, Revision: s.revision, Now: time.Now()}, nil
}
func (s *admissionStore) CompareAndSwap(_ context.Context, _, revision string, record sharedclaim.Record) error {
	if revision != s.revision {
		return sharedclaim.ErrConflict
	}
	s.writes++
	s.record, s.revision = record, fmt.Sprint(s.writes)
	if s.fail {
		return errors.New("ACK lost after provider commit")
	}
	return nil
}

func TestCoordinatedSharedAdmissionAndReleaseWaitForAcknowledgment(t *testing.T) {
	ledger, err := OpenClaimLedger(filepath.Join(t.TempDir(), "claims.json"))
	if err != nil {
		t.Fatal(err)
	}
	store := &admissionStore{fail: true}
	key := ClaimKey{Gaggle: "gaggle", Provider: "github", ExternalID: "42"}
	owner := sharedclaim.Owner{Instance: "instance", Run: "run", Token: "persisted-token"}
	claim := func() (bool, string, error) {
		return ledger.ClaimSharedScoped(t.Context(), store, "repository/42", key, owner, "implement", time.Minute)
	}
	if ok, _, err := claim(); err == nil || ok {
		t.Fatal("uncertain remote acquisition admitted local work")
	}
	if _, found := ledger.LookupScoped(key); found {
		t.Fatal("remote acquisition failure fell back to local claim")
	}
	store.fail = false
	if ok, _, err := claim(); err != nil || !ok {
		t.Fatalf("same-owner retry: %t %v", ok, err)
	}
	prior, _ := ledger.LookupScoped(key)
	path := ledger.path
	ledger.path = t.TempDir() // A directory cannot be replaced by the ledger file.
	if ok, _, err := claim(); err == nil || ok {
		t.Fatal("failed local persistence admitted a renewal")
	}
	ledger.path = path
	if store.record.Owner != owner {
		t.Fatal("failed local renewal released the previously active remote lease")
	}
	if current, found := ledger.LookupScoped(key); !found || !current.ExpiresAt.Equal(prior.ExpiresAt) {
		t.Fatal("failed renewal changed previously admitted local deadline")
	}
	store.fail = true
	if err := ledger.ReleaseCoordinatedShared(t.Context(), store, "repository/42", key, owner); err == nil {
		t.Fatal("uncertain release reported success")
	}
	if entry, found := ledger.LookupScoped(key); !found || entry.SharedOwner != owner {
		t.Fatal("uncertain release erased reconciliation owner")
	}
	store.fail = false
	if err := ledger.ReleaseCoordinatedShared(t.Context(), store, "repository/42", key, owner); err != nil {
		t.Fatal(err)
	}
	if _, found := ledger.LookupScoped(key); found {
		t.Fatal("acknowledged release left local claim")
	}
}
