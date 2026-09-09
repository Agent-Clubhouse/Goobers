package localscheduler

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sharedclaim"
)

// RevokeSharedExecution ends local execution authority without discarding
// provider cleanup custody. It needs no provider credential: even unavailable
// credentials must not keep an administratively revoked run executing.
func (l *ClaimLedger) RevokeSharedExecution(key ClaimKey, owner sharedclaim.Owner, actor string) (bool, error) {
	storageKey, err := key.storageKey()
	if err != nil {
		return false, err
	}
	return l.revokeSharedExecution(storageKey, owner, actor)
}

// ForceReleaseCoordinatedShared revokes execution durably before attempting
// remote cleanup. Failed cleanup retains both custody and the revocation; a
// renewal or retry by the old run must not restore execution authority. The
// caller holds the cross-process claims lock and resolves the exact persisted
// incarnation, never a caller-supplied replacement owner.
func (l *ClaimLedger) ForceReleaseCoordinatedShared(ctx context.Context, store sharedclaim.Store, remoteKey string, key ClaimKey, owner sharedclaim.Owner, actor string) error {
	storageKey, err := key.storageKey()
	if err != nil {
		return err
	}
	held, err := l.revokeSharedExecution(storageKey, owner, actor)
	if err != nil || !held {
		return err
	}
	if err := sharedclaim.Release(ctx, store, remoteKey, owner); err != nil {
		return err
	}
	return l.forceRelease(storageKey, actor, owner)
}

func (l *ClaimLedger) revokeSharedExecution(storageKey string, owner sharedclaim.Owner, actor string) (bool, error) {
	if actor == "" || owner == (sharedclaim.Owner{}) {
		return false, fmt.Errorf("shared revocation requires an actor and persisted owner")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, held := l.entries[storageKey]
	if !held {
		return false, nil
	}
	if entry.SharedOwner != owner || entry.SharedDeadline.IsZero() {
		return false, sharedclaim.ErrNotOwner
	}
	if entry.SharedRevoked {
		return true, nil
	}
	previousHistory, hadHistory := l.historyEntry(entry.RunID, storageKey)
	revoked := entry
	revoked.SharedRevoked = true
	// Older readers still stop at the shortened deadline. The explicit bit
	// also prevents an early release from being mistaken for voluntary release.
	if now := l.now(); revoked.SharedDeadline.After(now) {
		revoked.SharedDeadline = now
		revoked.ExpiresAt = now
	}
	l.entries[storageKey] = revoked
	l.recordHistory(storageKey, revoked)
	if err := l.persist(); err != nil {
		l.entries[storageKey] = entry
		l.restoreHistory(entry.RunID, storageKey, previousHistory, hadHistory)
		return false, err
	}
	l.journalWithRunner(journal.EventRunnerAnnotation, revoked, map[string]any{"operation": "claim_execution_revoked", "actor": actor})
	return true, nil
}
