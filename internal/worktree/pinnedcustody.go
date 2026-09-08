package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const pinnedCustodyFile = "pin.custody.json"

func (m *Manager) preparePinnedWithCustody(ctx context.Context, key string, opts PinnedOptions) (*Worktree, error) {
	if err := m.handoffPinnedState(ctx, key, ""); err != nil {
		return nil, err
	}
	workspace, err := m.preparePinned(ctx, key, opts)
	if err != nil {
		return nil, err
	}
	if err := m.recordPinnedCustody(key, opts); err != nil {
		return nil, err
	}
	return workspace, nil
}

// The custody marker outlives the lease: releasing a lease must not erase the
// identity needed to preserve the previous run before the next reset.
func (m *Manager) handoffPinnedState(ctx context.Context, key, expectedOwner string) error {
	root := filepath.Join(m.pinnedRoot, key)
	path := filepath.Join(root, "pin")
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	owner, err := readMarker(filepath.Join(root, pinnedCustodyFile))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if expectedOwner != "" && owner.OwnerRunID != expectedOwner {
		return fmt.Errorf("%w: pinned custody owner mismatch", ErrCleanupDeferred)
	}
	// Missing legacy ownership is passed explicitly to the guard, which may
	// refuse cleanup. Never substitute the next run's identity for old work.
	return m.prepareMarkerCleanup(ctx, path, "pin-"+key, owner)
}

func (m *Manager) recordPinnedCustody(key string, opts PinnedOptions) error {
	// An operator reset creates no implementation run. Keep the prior identity
	// until a real run takes custody; its preserved archive is independent.
	if opts.RunID == "workspace-reset" {
		return nil
	}
	return writeMarker(filepath.Join(m.pinnedRoot, key, pinnedCustodyFile), marker{
		RunID: opts.RunID, OwnerRunID: opts.RunID,
		RepositoryDigest: RepositoryDigest(opts.RepoURL), CreatedAt: time.Now().UTC(),
	})
}
