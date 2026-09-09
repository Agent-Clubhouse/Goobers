// Package sharedclaim implements the owner-scoped lease transitions used by
// opt-in cross-instance admission. Local visibility markers are not inputs.
package sharedclaim

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrHeld means another owner still holds the shared admission lease.
	ErrHeld = errors.New("shared claim is held by another owner")
	// ErrConflict means the observed revision changed before the transition.
	ErrConflict = errors.New("shared claim revision changed")
	// ErrNotOwner refuses release by a different claim incarnation.
	ErrNotOwner = errors.New("shared claim owner does not match")
)

// Owner includes an incarnation token persisted with the local lease. A reused
// run ID must not authorize an old process to release a newly acquired claim.
type Owner struct {
	Instance string `json:"instance"`
	Run      string `json:"run"`
	Token    string `json:"token"`
}

// Record is a shared coordination record, never a human-facing local mirror.
// An empty owner is a released tombstone: release advances rather than deletes
// the revision so a delayed writer cannot succeed against a recreated record.
type Record struct {
	Version   int       `json:"version"`
	Owner     Owner     `json:"owner"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Observation binds a record to its provider revision and provider clock.
// An absent record has an empty Revision. Provider time, not the worker clock,
// decides whether another instance's lease has expired.
type Observation struct {
	Record   Record
	Revision string
	Now      time.Time
}

// Store must atomically compare the exact observed revision before writing.
// CompareAndSwap returns ErrConflict on contention, never an unconditional
// overwrite. A lost response is an error; callers may retry with the same Owner.
type Store interface {
	Read(context.Context, string) (Observation, error)
	CompareAndSwap(context.Context, string, string, Record) error
}

// Acquire establishes or renews one incarnation. It does not itself admit a
// run: the caller must also establish its local ledger lease before execution.
func Acquire(ctx context.Context, store Store, key string, owner Owner, ttl time.Duration) error {
	if store == nil || key == "" || !validOwner(owner) || ttl <= 0 || ttl > 24*time.Hour {
		return fmt.Errorf("invalid shared claim acquisition")
	}
	observed, err := store.Read(ctx, key)
	if err != nil {
		return err
	}
	if err := validateObservation(observed); err != nil {
		return err
	}
	if observed.Record.Owner != (Owner{}) && observed.Record.Owner != owner && observed.Now.Before(observed.Record.ExpiresAt) {
		return ErrHeld
	}
	expires := observed.Now.Add(ttl)
	// An older renewal request may arrive after a newer, longer one. Never
	// shorten the same incarnation's lease: its local owner may still rely on
	// the previously acknowledged deadline.
	if observed.Record.Owner == owner && observed.Record.ExpiresAt.After(expires) {
		expires = observed.Record.ExpiresAt
	}
	return store.CompareAndSwap(ctx, key, observed.Revision, Record{Version: 1, Owner: owner, ExpiresAt: expires})
}

// Release clears only this incarnation. It retains a versioned tombstone and
// never treats another owner's lease as successfully released.
func Release(ctx context.Context, store Store, key string, owner Owner) error {
	if store == nil || key == "" || !validOwner(owner) {
		return fmt.Errorf("invalid shared claim release")
	}
	observed, err := store.Read(ctx, key)
	if err != nil {
		return err
	}
	if err := validateObservation(observed); err != nil {
		return err
	}
	if observed.Record.Owner == (Owner{}) {
		return nil
	}
	if observed.Record.Owner != owner {
		return ErrNotOwner
	}
	return store.CompareAndSwap(ctx, key, observed.Revision, Record{Version: 1})
}

func validOwner(owner Owner) bool {
	return owner.Instance != "" && owner.Run != "" && owner.Token != "" &&
		len(owner.Instance) <= 256 && len(owner.Run) <= 256 && len(owner.Token) <= 256
}

func validateObservation(observed Observation) error {
	if observed.Now.IsZero() {
		return fmt.Errorf("shared claim has no provider clock")
	}
	if observed.Revision == "" {
		if observed.Record != (Record{}) {
			return fmt.Errorf("unversioned shared claim record")
		}
		return nil
	}
	if observed.Record.Version != 1 {
		return fmt.Errorf("unsupported shared claim record")
	}
	if observed.Record.Owner == (Owner{}) {
		if !observed.Record.ExpiresAt.IsZero() {
			return fmt.Errorf("released shared claim retains a deadline")
		}
	} else if !validOwner(observed.Record.Owner) || observed.Record.ExpiresAt.IsZero() {
		return fmt.Errorf("invalid shared claim ownership")
	}
	return nil
}
