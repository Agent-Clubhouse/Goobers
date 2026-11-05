package claimsclient

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/sharedclaim"
)

type guardedExpiryLedger struct {
	*localscheduler.ClaimLedger
	check func()
}

func (l *guardedExpiryLedger) BeginSharedRelease(entry Entry, expiredAt time.Time) (bool, error) {
	l.check()
	return l.ClaimLedger.BeginSharedRelease(entry, expiredAt)
}

func TestSharedExpiryReservationHoldsBothLocksAndReportsOnlyRemoval(t *testing.T) {
	store := &sharedClientStore{}
	resolver := &sharedClientResolver{binding: SharedClaimBinding{Store: store, RemoteKey: "42",
		Owner: sharedclaim.Owner{Instance: "instance", Run: "run", Token: "token"}}}
	cfg := sharedTestFileConfig(filepath.Join(t.TempDir(), "claims.json"), resolver)
	var claimsHeld, transitionHeld bool
	claimsLock, transitionLock := cfg.Lock, cfg.ProviderReleaseLock
	cfg.Lock = func(op string, fn func() error) error {
		return claimsLock(op, func() error {
			claimsHeld = true
			defer func() { claimsHeld = false }()
			return fn()
		})
	}
	cfg.ProviderReleaseLock = func(key Key, fn func() error) error {
		return transitionLock(key, func() error {
			transitionHeld = true
			defer func() { transitionHeld = false }()
			return fn()
		})
	}
	checks := 0
	cfg.Open = func(path string, opts ...localscheduler.LedgerOption) (FileLedger, error) {
		ledger, err := localscheduler.OpenClaimLedger(path, opts...)
		return &guardedExpiryLedger{ClaimLedger: ledger, check: func() {
			checks++
			if !claimsHeld || !transitionHeld {
				t.Fatal("expiry reservation did not hold both transition and claims locks")
			}
		}}, err
	}
	client, err := NewFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	key := Key{Gaggle: "g", Provider: "github", ExternalID: "42"}
	if ok, _, err := client.ClaimScoped(t.Context(), key, "run", "implement", time.Minute); err != nil || !ok {
		t.Fatalf("acquire: %v %v", ok, err)
	}
	entries, err := client.ForRunAll(t.Context(), "run")
	if err != nil || len(entries) != 1 {
		t.Fatalf("snapshot: %+v %v", entries, err)
	}
	entry := entries[0]
	reason := claimrelease.StallSwept.Reason()
	if released, err := client.ReleaseExpiredExpected(t.Context(), entry, reason, time.Now()); err != nil || released {
		t.Fatalf("unexpired cleanup: %v %v", released, err)
	}
	if store.writes != 1 {
		t.Fatal("unexpired cleanup mutated remote custody")
	}
	// The exact cutoff is expired; acknowledged removal is reported once.
	if released, err := client.ReleaseExpiredExpected(t.Context(), entry, reason, entry.ExpiresAt); err != nil || !released {
		t.Fatalf("expired cleanup: %v %v", released, err)
	}
	if released, err := client.ReleaseExpiredExpected(t.Context(), entry, reason, entry.ExpiresAt); err != nil || released {
		t.Fatalf("already absent cleanup: %v %v", released, err)
	}
	if checks != 3 || store.writes != 2 {
		t.Fatalf("reservation checks=%d remote writes=%d, want 3 and 2", checks, store.writes)
	}
}
