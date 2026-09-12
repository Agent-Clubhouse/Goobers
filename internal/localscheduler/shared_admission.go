package localscheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/sharedclaim"
)

// ClaimSharedScoped admits only after remote CAS and durable local admission
// both succeed. The caller binds remoteKey to the configured repository/item,
// persists owner independently for lost-ACK retries, and fences execution by
// the stored SharedDeadline. This method never falls back to local admission.
func (l *ClaimLedger) ClaimSharedScoped(ctx context.Context, store sharedclaim.Store, remoteKey string, key ClaimKey, owner sharedclaim.Owner, workflow string, ttl time.Duration) (bool, string, error) {
	storageKey, err := key.storageKey()
	if err != nil {
		return false, "", err
	}
	l.mu.Lock()
	historical, _ := l.historyEntry(owner.Run, storageKey)
	l.mu.Unlock()
	if historical.SharedRevoked {
		return false, "", fmt.Errorf("shared execution was administratively revoked")
	}
	deadline, err := sharedclaim.AcquireUntil(ctx, store, remoteKey, owner, ttl)
	if err != nil {
		return false, "", err
	}
	// Do not release the remote lease if local persistence fails: this may
	// be a renewal of an already admitted owner. Releasing it could invalidate
	// execution still relying on its previously acknowledged deadline.
	ok, holder, err := l.ClaimScopedUntil(key, owner.Run, workflow, deadline, owner)
	if err != nil || !ok {
		return ok, holder, err
	}
	if err := ctx.Err(); err != nil {
		return false, "", err
	}
	if !time.Now().Before(deadline) {
		return false, "", fmt.Errorf("shared admission expired during local persistence")
	}
	return true, holder, nil
}

// ReleaseCoordinatedShared retains the local incarnation until the provider
// acknowledges its owner-scoped release or a fresh validated read proves that
// incarnation is already gone. A successor is never released. An uncertain ACK
// retains custody for reconciliation after restart.
func (l *ClaimLedger) ReleaseCoordinatedShared(ctx context.Context, store sharedclaim.Store, remoteKey string, key ClaimKey, owner sharedclaim.Owner) error {
	if _, err := key.storageKey(); err != nil {
		return err
	}
	if err := releaseSharedCustody(ctx, store, remoteKey, owner); err != nil {
		return err
	}
	return l.ReleaseSharedScoped(key, owner)
}

func releaseSharedCustody(ctx context.Context, store sharedclaim.Store, remoteKey string, owner sharedclaim.Owner) error {
	err := sharedclaim.Release(ctx, store, remoteKey, owner)
	if errors.Is(err, sharedclaim.ErrNotOwner) {
		// Retire only local custody, with a second authoritative observation.
		// Network errors, malformed records, and a return to our incarnation
		// remain failures; none are evidence that cleanup is complete.
		return sharedclaim.ConfirmOwnerGone(ctx, store, remoteKey, owner)
	}
	return err
}
