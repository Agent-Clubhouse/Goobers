package main

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/sharedclaim"
)

func TestRecoveryPublicationReleasedClaimDispositionFence(t *testing.T) {
	for _, disposition := range []claimrelease.Disposition{
		claimrelease.Completed, claimrelease.Failed, claimrelease.Escalated,
		claimrelease.Cancelled, claimrelease.Abandoned, claimrelease.NoWork,
		claimrelease.StallSwept, claimrelease.Reconciled, claimrelease.ForceReleased,
		claimrelease.Superseded, claimrelease.Unknown,
	} {
		t.Run(string(disposition), func(t *testing.T) {
			ledger, err := localscheduler.OpenClaimLedger(filepath.Join(t.TempDir(), "claims.json"))
			if err != nil {
				t.Fatal(err)
			}
			if ok, _, err := ledger.Claim("7", "source-run", "implementation", time.Hour); err != nil || !ok {
				t.Fatalf("claim: %t %v", ok, err)
			}
			original := ledger.ForRunAll("source-run")[0]
			if disposition == claimrelease.ForceReleased {
				err = ledger.ForceRelease("7")
			} else {
				err = ledger.Release("7", "source-run", disposition.Reason())
			}
			if err != nil {
				t.Fatal(err)
			}
			claim, admitted := recoveryDeliveryClaim(ledger, "source-run", "7", time.Now().UTC(), true)
			want := disposition == claimrelease.Completed || disposition == claimrelease.Failed || disposition == claimrelease.Escalated
			if admitted != want {
				t.Fatalf("publication after %s release admitted=%t, want %t", disposition, admitted, want)
			}
			if admitted && !claim.ExpiresAt.Equal(original.ExpiresAt) {
				t.Fatalf("late publication changed lease deadline: got %s, want %s", claim.ExpiresAt, original.ExpiresAt)
			}
			if _, admitted := recoveryDeliveryClaim(ledger, "source-run", "7", time.Now().UTC(), false); admitted {
				t.Fatal("released lease authorized a recovery download")
			}
			if _, admitted := recoveryDeliveryClaim(ledger, "source-run", "7", original.ExpiresAt, true); admitted {
				t.Fatal("expired released lease authorized publication")
			}
		})
	}
}

func TestRecoveryPublicationReleasedSharedClaimCannotBypassRemoteOwnership(t *testing.T) {
	for _, successorOwns := range []bool{false, true} {
		t.Run(fmt.Sprintf("successor=%t", successorOwns), func(t *testing.T) {
			ledger, err := localscheduler.OpenClaimLedger(filepath.Join(t.TempDir(), "claims.json"))
			if err != nil {
				t.Fatal(err)
			}
			store := &recoveryFenceSharedStore{}
			key := localscheduler.ClaimKey{Gaggle: "gaggle", Provider: "github", ExternalID: "7"}
			owner := sharedclaim.Owner{Instance: "instance-a", Run: "source-run", Token: "token-a"}
			if ok, _, err := ledger.ClaimSharedScoped(t.Context(), store, "repository/7", key, owner, "implementation", time.Hour); err != nil || !ok {
				t.Fatalf("claim: %t %v", ok, err)
			}
			if err := ledger.ReleaseCoordinatedShared(t.Context(), store, "repository/7", key, owner, claimrelease.Failed.Reason()); err != nil {
				t.Fatal(err)
			}
			if successorOwns {
				successorLedger, err := localscheduler.OpenClaimLedger(filepath.Join(t.TempDir(), "claims.json"))
				if err != nil {
					t.Fatal(err)
				}
				successor := sharedclaim.Owner{Instance: "instance-b", Run: "successor-run", Token: "token-b"}
				if ok, _, err := successorLedger.ClaimSharedScoped(t.Context(), store, "repository/7", key, successor, "implementation", time.Hour); err != nil || !ok {
					t.Fatalf("successor claim: %t %v", ok, err)
				}
				if store.record.Owner != successor {
					t.Fatal("successor did not own remote claim")
				}
			}
			if _, admitted := recoveryDeliveryClaim(ledger, owner.Run, "7", time.Now().UTC(), true); admitted {
				t.Fatal("released shared claim authorized publication through local-only fencing")
			}
		})
	}
}

type recoveryFenceSharedStore struct {
	record   sharedclaim.Record
	revision string
	writes   int
}

func (s *recoveryFenceSharedStore) Read(context.Context, string) (sharedclaim.Observation, error) {
	return sharedclaim.Observation{Record: s.record, Revision: s.revision, Now: time.Now()}, nil
}

func (s *recoveryFenceSharedStore) CompareAndSwap(_ context.Context, _, revision string, record sharedclaim.Record) error {
	if revision != s.revision {
		return sharedclaim.ErrConflict
	}
	s.writes++
	s.record, s.revision = record, fmt.Sprint(s.writes)
	return nil
}
