package worktree

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"
)

// ErrCleanupDeferred means a durable handoff has not completed. The source
// must remain intact; housekeeping may report a warning and try other targets,
// but direct removal/replacement must still fail.
var ErrCleanupDeferred = errors.New("worktree cleanup deferred pending durable handoff")

// CleanupTarget identifies the directory about to be destroyed. OwnerRunID
// comes from its durable marker; an empty value must not be guessed from the
// worktree ID, since run IDs and stage names can both contain hyphens.
type CleanupTarget struct {
	Path       string
	WorktreeID string
	OwnerRunID string
	// RepositoryDigest and CreatedAt are copied from the durable marker.
	// Empty values identify legacy metadata and must not be guessed.
	RepositoryDigest string
	CreatedAt        time.Time
}

// WithBeforeCleanup installs a durable-evidence handoff before destructive
// cleanup, including reserved-branch rollback. A failure preserves the directory
// and ownership records. The callback runs under the repository lock and must
// not re-enter Manager. Configure at construction; different repos may call
// concurrently. Multiple options are composed in their registration order.
func WithBeforeCleanup(callback func(context.Context, CleanupTarget) error) ManagerOption {
	return func(m *Manager) {
		if callback == nil {
			return
		}
		previous := m.beforeCleanup
		m.beforeCleanup = func(ctx context.Context, target CleanupTarget) error {
			if previous != nil {
				if err := previous(ctx, target); err != nil {
					return err
				}
			}
			return callback(ctx, target)
		}
	}
}

func (m *Manager) prepareCleanup(ctx context.Context, path, worktreeID, ownerRunID string) error {
	return m.prepareCleanupTarget(ctx, CleanupTarget{Path: path, WorktreeID: worktreeID, OwnerRunID: ownerRunID})
}

func (m *Manager) prepareMarkerCleanup(ctx context.Context, path, worktreeID string, mk marker) error {
	return m.prepareCleanupTarget(ctx, CleanupTarget{Path: path, WorktreeID: worktreeID, OwnerRunID: mk.OwnerRunID, RepositoryDigest: mk.RepositoryDigest, CreatedAt: mk.CreatedAt})
}

func (m *Manager) prepareCleanupTarget(ctx context.Context, target CleanupTarget) error {
	// Snapshot reloadable guards before invoking any callback. No registry
	// mutex is held during potentially slow archive/journal publication.
	m.cleanupGuardsMu.RLock()
	guards := maps.Clone(m.cleanupGuards)
	m.cleanupGuardsMu.RUnlock()
	if m.beforeCleanup != nil {
		if err := m.beforeCleanup(ctx, target); err != nil {
			return fmt.Errorf("%w: preserve evidence for %s: %w", ErrCleanupDeferred, target.WorktreeID, err)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(guards)) {
		if err := guards[name](ctx, target); err != nil {
			return fmt.Errorf("%w: %s handoff for %s: %w", ErrCleanupDeferred, name, target.WorktreeID, err)
		}
	}
	return nil
}

// SetCleanupGuard atomically installs or replaces one named cleanup handoff.
// It preserves construction-time callbacks and other named guards. A cleanup
// already underway completes with its captured guard set; subsequent cleanup
// uses the new set. Guards execute in name order under the repository lock and
// must not re-enter Manager operations requiring that lock.
func (m *Manager) SetCleanupGuard(name string, callback func(context.Context, CleanupTarget) error) error {
	if name == "" || callback == nil {
		return fmt.Errorf("cleanup guard requires a name and callback")
	}
	m.cleanupGuardsMu.Lock()
	defer m.cleanupGuardsMu.Unlock()
	if m.cleanupGuards == nil {
		m.cleanupGuards = make(map[string]func(context.Context, CleanupTarget) error)
	}
	m.cleanupGuards[name] = callback
	return nil
}
