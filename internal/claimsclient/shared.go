package claimsclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/sharedclaim"
)

// SharedClaimBinding is resolved from trusted instance/run configuration,
// never from a stage-requested mode or incarnation token.
type SharedClaimBinding struct {
	Store     sharedclaim.Store
	RemoteKey string
	Owner     sharedclaim.Owner
	// BeforeTransition durably registers retry custody before a remote write
	// can outlive this process. Failure must prevent the remote transition.
	BeforeTransition func(context.Context) error
	// AfterTransition repairs non-authoritative visibility, including after an
	// uncertain remote ACK. It must be bounded, must not acquire the transition
	// lock, and must report its own errors without changing the claim result.
	AfterTransition func(context.Context)
}

// SharedClaimResolver supplies the provider binding at the file-client seam.
// Admission returns nil only for an explicitly resolved local policy. Release
// must resolve the persisted binding even when its owning run is terminal;
// current workflow settings must not turn a shared release into local cleanup.
// Both methods run outside the claims lock. Provider setup may perform I/O.
type SharedClaimResolver interface {
	Admission(context.Context, Key, string, string) (*SharedClaimBinding, error)
	Release(context.Context, Entry) (SharedClaimBinding, error)
}

// ErrSharedRequiresUnlocked prevents a caller-owned claims lock from enclosing
// provider I/O. Shared transitions must use the owning File, not a held session.
var ErrSharedRequiresUnlocked = errors.New("claimsclient: shared transitions require an unlocked file client")

type sharedTransitionLedger interface {
	BeginSharedAdmission(Key, sharedclaim.Owner, string) (Entry, bool, string, error)
	FinishSharedAdmission(Entry, time.Time) (bool, string, error)
	BeginSharedRelease(Entry, time.Time) (bool, error)
	FinishProviderRelease(Entry, claimrelease.Reason) (bool, error)
}

func (f *File) sharedTransition(key Key, fn func() error) error {
	if f.cfg.Lock == nil || f.cfg.ProviderReleaseLock == nil {
		return ErrSharedRequiresUnlocked
	}
	return f.cfg.ProviderReleaseLock(key, fn)
}

func sharedLedger(tx Ledger) (sharedTransitionLedger, error) {
	ledger, ok := tx.(*fileSession).ledger.(sharedTransitionLedger)
	if !ok {
		return nil, fmt.Errorf("claimsclient: shared transition reservation is unavailable")
	}
	return ledger, nil
}

func (f *File) claimShared(ctx context.Context, key Key, runID, workflow string, ttl time.Duration, binding SharedClaimBinding, expected *Entry) (ok bool, holder string, err error) {
	if binding.Owner.Run != runID {
		return false, "", fmt.Errorf("claimsclient: shared admission belongs to another run")
	}
	if ttl <= time.Second || ttl > 24*time.Hour {
		return false, "", fmt.Errorf("invalid shared claim TTL")
	}
	err = f.sharedTransition(key, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if binding.BeforeTransition != nil {
			if err := binding.BeforeTransition(ctx); err != nil {
				return err
			}
		}
		var reservedEntry Entry
		var reserved bool
		if err := f.Locked(ctx, "claims.shared.reserve", func(tx Ledger) error {
			ledger, err := sharedLedger(tx)
			if err != nil {
				return err
			}
			if expected != nil {
				current, held := matchingEntry(tx.(*fileSession).ledger, *expected)
				if !held || !current.ExpiresAt.After(time.Now()) || current.ReleasePending || current.SharedRevoked {
					holder = current.RunID
					return nil
				}
			}
			reservedEntry, reserved, holder, err = ledger.BeginSharedAdmission(key, binding.Owner, workflow)
			return err
		}); err != nil || !reserved {
			return err
		}
		if binding.AfterTransition != nil {
			defer binding.AfterTransition(ctx)
		}
		deadline, err := sharedclaim.AcquireUntil(ctx, binding.Store, binding.RemoteKey, binding.Owner, ttl)
		if err != nil {
			return err
		}
		return f.Locked(ctx, "claims.shared.finalize", func(tx Ledger) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			ledger, err := sharedLedger(tx)
			if err != nil {
				return err
			}
			ok, holder, err = ledger.FinishSharedAdmission(reservedEntry, deadline)
			return err
		})
	})
	return ok, holder, err
}

func (f *File) releaseShared(ctx context.Context, entry Entry, reason claimrelease.Reason, expiredAt time.Time) (released bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if f.cfg.Shared == nil {
		return false, fmt.Errorf("claimsclient: shared release requires its provider resolver")
	}
	err = f.sharedTransition(KeyForEntry(entry), func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		binding, err := f.cfg.Shared.Release(ctx, entry)
		if err != nil {
			return err
		}
		if binding.Owner != entry.SharedOwner {
			return fmt.Errorf("claimsclient: shared release belongs to another incarnation")
		}
		if binding.BeforeTransition != nil {
			if err := binding.BeforeTransition(ctx); err != nil {
				return err
			}
		}
		var reserved bool
		if err := f.Locked(ctx, "claims.shared.release.reserve", func(tx Ledger) error {
			ledger, err := sharedLedger(tx)
			if err != nil {
				return err
			}
			reserved, err = ledger.BeginSharedRelease(entry, expiredAt)
			return err
		}); err != nil || !reserved {
			return err
		}
		if binding.AfterTransition != nil {
			defer binding.AfterTransition(ctx)
		}
		err = sharedclaim.Release(ctx, binding.Store, binding.RemoteKey, binding.Owner)
		if errors.Is(err, sharedclaim.ErrNotOwner) {
			err = sharedclaim.ConfirmOwnerGone(ctx, binding.Store, binding.RemoteKey, binding.Owner)
		}
		if err != nil {
			return err
		}
		return f.Locked(ctx, "claims.shared.release.finalize", func(tx Ledger) error {
			ledger, err := sharedLedger(tx)
			if err != nil {
				return err
			}
			released, err = ledger.FinishProviderRelease(entry, reason)
			return err
		})
	})
	return released, err
}

func (s *fileSession) releaseEntry(_ context.Context, entry Entry, runID string, reason claimrelease.Reason) error {
	if !entry.SharedDeadline.IsZero() {
		return ErrSharedRequiresUnlocked
	}
	return s.ledger.ReleaseEntry(entry, runID, reason)
}
