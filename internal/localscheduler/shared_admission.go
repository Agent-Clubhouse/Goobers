package localscheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/sharedclaim"
)

// ClaimSharedScoped admits only after remote CAS and durable local admission
// both succeed. The caller binds remoteKey to the configured repository/item,
// persists owner independently for lost-ACK retries, and fences execution by
// the stored SharedDeadline. This method never falls back to local admission.
func (l *ClaimLedger) ClaimSharedScoped(ctx context.Context, store sharedclaim.Store, remoteKey string, key ClaimKey, owner sharedclaim.Owner, workflow string, ttl time.Duration) (bool, string, error) {
	if _, err := key.storageKey(); err != nil {
		return false, "", err
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
// acknowledges its owner-scoped release. An uncertain ACK is not success and
// does not erase the evidence needed to reconcile after restart.
func (l *ClaimLedger) ReleaseCoordinatedShared(ctx context.Context, store sharedclaim.Store, remoteKey string, key ClaimKey, owner sharedclaim.Owner) error {
	if _, err := key.storageKey(); err != nil {
		return err
	}
	if err := sharedclaim.Release(ctx, store, remoteKey, owner); err != nil {
		return err
	}
	return l.ReleaseSharedScoped(key, owner)
}
