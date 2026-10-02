package readservice

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
)

// "What is actively claimed now" (#1488).
//
// The claim ledger (scheduler/claims.json) is this instance's shared claim
// record: every workflow and process on the instance claims through it, and a
// claim made under shared visibility carries its coordination owner there too.
// This view answers one question over it — which items are held right now, by
// which run, and for how long — without the lease bookkeeping `claims list`
// prints. Other instances' claims are out of scope: only this ledger is read.

// ActiveClaimsReader is the optional read surface behind GET /api/v1/claims/active.
type ActiveClaimsReader interface {
	ActiveClaims(context.Context) (ActiveClaimList, error)
}

// ActiveClaimHolderLocal names the holder of a claim made without shared
// visibility: this instance's own scheduler.
const ActiveClaimHolderLocal = "local"

// ActiveClaimList is the instance's currently held claims, oldest first.
type ActiveClaimList struct {
	ObservedAt time.Time     `json:"observedAt"`
	Claims     []ActiveClaim `json:"claims"`
}

// ActiveClaim is one backlog item a run holds right now.
type ActiveClaim struct {
	ItemID   string `json:"itemId"`
	Gaggle   string `json:"gaggle,omitempty"`
	Provider string `json:"provider,omitempty"`
	Workflow string `json:"workflow"`
	RunID    string `json:"runId"`
	// Holder is the instance that owns the claim: the shared coordination
	// owner when the claim was made with shared visibility, otherwise
	// ActiveClaimHolderLocal. The owner's incarnation token is never exposed.
	Holder     string    `json:"holder"`
	Shared     bool      `json:"shared"`
	ClaimedAt  time.Time `json:"claimedAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
	AgeSeconds int64     `json:"ageSeconds"`
}

// ActiveClaimsFromEntries reduces ledger entries to the claims held at now.
//
// A lease that has expired, been released, or had its shared execution
// authority revoked is not an active claim: the item is no longer being
// worked under it, even while the entry lingers for recovery to sweep.
func ActiveClaimsFromEntries(entries []localscheduler.ClaimEntry, now time.Time) []ActiveClaim {
	claims := make([]ActiveClaim, 0, len(entries))
	for _, entry := range entries {
		if entry.ReleasedAt != nil || entry.SharedRevoked || !entry.ExpiresAt.After(now) {
			continue
		}
		holder, shared := ActiveClaimHolderLocal, false
		if entry.SharedOwner.Instance != "" {
			holder, shared = entry.SharedOwner.Instance, true
		}
		age := now.Sub(entry.ClaimedAt)
		if age < 0 {
			age = 0
		}
		claims = append(claims, ActiveClaim{
			ItemID:     entry.ItemID,
			Gaggle:     entry.Gaggle,
			Provider:   entry.Provider,
			Workflow:   entry.Workflow,
			RunID:      entry.RunID,
			Holder:     holder,
			Shared:     shared,
			ClaimedAt:  entry.ClaimedAt.UTC(),
			ExpiresAt:  entry.ExpiresAt.UTC(),
			AgeSeconds: int64(age / time.Second),
		})
	}
	sort.SliceStable(claims, func(i, j int) bool {
		if !claims[i].ClaimedAt.Equal(claims[j].ClaimedAt) {
			return claims[i].ClaimedAt.Before(claims[j].ClaimedAt)
		}
		return claims[i].ItemID < claims[j].ItemID
	})
	return claims
}

// ActiveClaims reads the instance claim ledger without taking its lock.
//
// The ledger is replaced atomically on every mutation, so an unlocked read
// sees one whole committed state; it may be a moment behind a concurrent
// claim, which is the same freshness every other read route offers.
func (s *Local) ActiveClaims(context.Context) (ActiveClaimList, error) {
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(s.sources.Layout.SchedulerDir(), "claims.json"))
	if err != nil {
		return ActiveClaimList{}, fmt.Errorf("read claim ledger: %w", err)
	}
	now := s.now().UTC()
	return ActiveClaimList{ObservedAt: now, Claims: ActiveClaimsFromEntries(ledger.Snapshot(), now)}, nil
}
