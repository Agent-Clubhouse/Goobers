package worktree

import (
	"context"
	"fmt"
)

// CleanupTarget identifies the directory about to be destroyed. OwnerRunID
// comes from its durable marker; an empty value must not be guessed from the
// worktree ID, since run IDs and stage names can both contain hyphens.
type CleanupTarget struct {
	Path       string
	WorktreeID string
	OwnerRunID string
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
	if m.beforeCleanup == nil {
		return nil
	}
	if err := m.beforeCleanup(ctx, CleanupTarget{Path: path, WorktreeID: worktreeID, OwnerRunID: ownerRunID}); err != nil {
		return fmt.Errorf("worktree: preserve evidence before cleanup of %s: %w", worktreeID, err)
	}
	return nil
}
