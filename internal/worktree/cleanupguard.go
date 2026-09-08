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

// WithBeforeCleanup installs the durable-evidence handoff required before
// teardown, orphan reaping, retention pruning, or replacement of a stale
// attempt. A failure preserves the directory and its ownership records.
// The callback runs under the repository lock and must not re-enter Manager.
// Configure it at construction; callbacks may run concurrently across repos.
func WithBeforeCleanup(callback func(context.Context, CleanupTarget) error) ManagerOption {
	return func(m *Manager) { m.beforeCleanup = callback }
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
