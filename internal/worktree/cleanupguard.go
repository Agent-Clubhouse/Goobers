package worktree

import (
	"context"
	"fmt"
	"time"
)

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
	if m.beforeCleanup == nil {
		return nil
	}
	if err := m.beforeCleanup(ctx, target); err != nil {
		return fmt.Errorf("worktree: preserve evidence before cleanup of %s: %w", target.WorktreeID, err)
	}
	return nil
}
