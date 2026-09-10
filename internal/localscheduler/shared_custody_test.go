package localscheduler

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/sharedclaim"
)

type custodyObservationStore struct {
	first, second sharedclaim.Observation
	secondErr     error
	reads, writes int
}

func (s *custodyObservationStore) Read(context.Context, string) (sharedclaim.Observation, error) {
	s.reads++
	if s.reads == 1 {
		return s.first, nil
	}
	return s.second, s.secondErr
}
func (s *custodyObservationStore) CompareAndSwap(context.Context, string, string, sharedclaim.Record) error {
	s.writes++
	return errors.New("must not write a successor")
}

func TestSharedCustodyRetiresOnlyAfterValidatedOwnerLoss(t *testing.T) {
	for _, admin := range []bool{false, true} {
		for _, scenario := range []string{"successor", "released", "absent", "owner-returned", "read-error", "malformed", "missing-clock"} {
			name := scenario
			if admin {
				name = "admin/" + name
			}
			t.Run(name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "claims.json")
				ledger, err := OpenClaimLedger(path)
				if err != nil {
					t.Fatal(err)
				}
				key := ClaimKey{Gaggle: "g", Provider: "github", ExternalID: "42"}
				owner := sharedclaim.Owner{Instance: "old-instance", Run: "old-run", Token: "old-token"}
				if ok, _, err := ledger.ClaimScopedUntil(key, owner.Run, "work", time.Now().Add(time.Minute), owner); err != nil || !ok {
					t.Fatalf("seed: %v %v", ok, err)
				}
				successor := sharedclaim.Owner{Instance: "new-instance", Run: "new-run", Token: "new-token"}
				observed := sharedclaim.Observation{Now: time.Now(), Revision: "successor", Record: sharedclaim.Record{Version: 1, Owner: successor, ExpiresAt: time.Now().Add(time.Minute)}}
				store := &custodyObservationStore{first: observed, second: observed}
				switch scenario {
				case "released":
					store.second.Record = sharedclaim.Record{Version: 1}
				case "absent":
					store.second.Revision = ""
					store.second.Record = sharedclaim.Record{}
				case "owner-returned":
					store.second.Record.Owner = owner
				case "read-error":
					store.secondErr = errors.New("provider unavailable")
				case "malformed":
					store.second.Record.Version = 2
				case "missing-clock":
					store.second.Now = time.Time{}
				}
				if admin {
					err = ledger.ForceReleaseCoordinatedShared(t.Context(), store, "42", key, owner, "cli")
				} else {
					err = ledger.ReleaseCoordinatedShared(t.Context(), store, "42", key, owner)
				}
				wantReleased := scenario == "successor" || scenario == "released" || scenario == "absent"
				if (err == nil) != wantReleased || store.writes != 0 || store.reads != 2 {
					t.Fatalf("custody outcome: %v reads=%d writes=%d", err, store.reads, store.writes)
				}
				reopened, err := OpenClaimLedger(path)
				if err != nil {
					t.Fatal(err)
				}
				entry, held := reopened.LookupScoped(key)
				if held == wantReleased || (held && entry.SharedOwner != owner) {
					t.Fatalf("wrong durable custody: %+v held=%v", entry, held)
				}
				if admin && held && !entry.SharedRevoked {
					t.Fatal("failed cleanup lost administrative revocation")
				}
			})
		}
	}
}

func TestSharedCustodyLocalPersistenceFailurePreservesRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	ledger, err := OpenClaimLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	key := ClaimKey{Gaggle: "g", Provider: "github", ExternalID: "42"}
	owner := sharedclaim.Owner{Instance: "old", Run: "old-run", Token: "old-token"}
	if ok, _, err := ledger.ClaimScopedUntil(key, owner.Run, "work", time.Now().Add(time.Minute), owner); err != nil || !ok {
		t.Fatalf("seed: %v %v", ok, err)
	}
	observed := sharedclaim.Observation{Now: time.Now(), Revision: "next", Record: sharedclaim.Record{Version: 1, Owner: sharedclaim.Owner{Instance: "next", Run: "next-run", Token: "next-token"}, ExpiresAt: time.Now().Add(time.Minute)}}
	store := &custodyObservationStore{first: observed, second: observed}
	ledger.path = t.TempDir()
	if err := ledger.ReleaseCoordinatedShared(t.Context(), store, "42", key, owner); err == nil || store.writes != 0 {
		t.Fatalf("failed persistence reported success: %v", err)
	}
	if current, held := ledger.LookupScoped(key); !held || current.SharedOwner != owner {
		t.Fatal("failed persistence lost in-memory retry custody")
	}
	ledger.path = path
	store.reads = 0
	if err := ledger.ReleaseCoordinatedShared(t.Context(), store, "42", key, owner); err != nil {
		t.Fatal(err)
	}
}
