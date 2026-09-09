package worktree

import (
	"context"
	"fmt"
	"maps"
	"slices"
)

// CleanupTarget identifies the directory about to be destroyed. OwnerRunID
// comes from its durable marker; an empty value must not be guessed from the
// worktree ID, since run IDs and stage names can both contain hyphens.
type CleanupTarget struct {
	Path       string
	WorktreeID string
	OwnerRunID string
}

// MutationReceiptGuard identifies the handoff that authorizes retiring a
// mutation sidecar. An unrelated recovery handoff cannot acknowledge receipts.
const MutationReceiptGuard = "mutation-receipts"

// WithMutationReceiptCleanup installs the receipt-specific durable handoff.
func WithMutationReceiptCleanup(callback func(context.Context, CleanupTarget) error) ManagerOption {
	return func(m *Manager) {
		if callback != nil {
			_ = m.SetCleanupGuard(MutationReceiptGuard, callback)
		}
	}
}

// SetCleanupGuard installs one handoff without replacing other guards. Each
// cleanup snapshots the registry; callbacks run without its mutex held.
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

func (m *Manager) prepareCleanup(ctx context.Context, path, worktreeID, ownerRunID string) error {
	_, err := m.prepareCleanupWithReceipts(ctx, path, worktreeID, ownerRunID)
	return err
}

func (m *Manager) prepareCleanupWithReceipts(ctx context.Context, path, worktreeID, ownerRunID string) (bool, error) {
	m.cleanupGuardsMu.RLock()
	guards := maps.Clone(m.cleanupGuards)
	m.cleanupGuardsMu.RUnlock()
	receipts := false
	for _, name := range slices.Sorted(maps.Keys(guards)) {
		if err := guards[name](ctx, CleanupTarget{Path: path, WorktreeID: worktreeID, OwnerRunID: ownerRunID}); err != nil {
			return false, fmt.Errorf("worktree: %s handoff before cleanup of %s: %w", name, worktreeID, err)
		}
		receipts = receipts || name == MutationReceiptGuard
	}
	return receipts, nil
}
