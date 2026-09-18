package worktree

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/platform/safeopen"
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
	if err := m.recordPinnedCustody(ctx, key, opts); err != nil {
		return nil, err
	}
	return workspace, nil
}

// The custody marker outlives the lease: releasing a lease must not erase the
// identity needed to preserve the previous run before the next reset.
func (m *Manager) handoffPinnedState(ctx context.Context, key, expectedOwner string) error {
	root := filepath.Join(m.pinnedRoot, key)
	path := filepath.Join(root, "pin")
	if info, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	} else if !info.IsDir() {
		return fmt.Errorf("%w: pinned workspace must be a real directory", ErrCleanupDeferred)
	}
	owner, err := readPinnedCustody(root)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if expectedOwner != "" && owner.OwnerRunID != expectedOwner {
		return fmt.Errorf("%w: pinned custody owner mismatch", ErrCleanupDeferred)
	}
	// Missing legacy ownership is passed explicitly to the guard, which may
	// refuse cleanup. Never substitute the next run's identity for old work.
	return m.preparePreservedTarget(ctx, CleanupTarget{
		Path: path, WorktreeID: "pin-" + key, OwnerRunID: owner.OwnerRunID,
		Gaggle: owner.Gaggle, BaseRef: owner.BaseRef, RepositoryDigest: owner.RepositoryDigest,
		CreatedAt: owner.CreatedAt, Pinned: true,
	})
}

// WithPinnedWorkspaceOwnedBy visits a released pinned workspace under its
// repository and lease locks after verifying recovery custody and branch.
func (m *Manager) WithPinnedWorkspaceOwnedBy(ctx context.Context, repoURL, ownerRunID, branch string, visit func(string) error) (bool, error) {
	branch = strings.TrimSpace(branch)
	if repoURL == "" || !validRunID(ownerRunID) || branch == "" || visit == nil {
		return false, fmt.Errorf("worktree: pinned custody visit requires repository identity, run ID, branch, and visitor")
	}
	key := repoKey(repoURL)
	lock := m.lockFor(key)
	lock.Lock()
	defer lock.Unlock()
	root := filepath.Join(m.pinnedRoot, key)
	owner, err := readPinnedCustody(root)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if owner.OwnerRunID != ownerRunID {
		return false, nil
	}
	return m.withPinnedRecoveryRepository(ctx, repoURL, nil, func(repositories []string) error {
		pin := repositories[0]
		head, err := gitOutput(ctx, pin, "symbolic-ref", "--quiet", "HEAD")
		if err != nil {
			return fmt.Errorf("worktree: inspect pinned recovery branch: %w", err)
		}
		if head != "refs/heads/"+branch {
			return fmt.Errorf("worktree: pinned recovery workspace is on branch %q, expected %q", strings.TrimPrefix(head, "refs/heads/"), branch)
		}
		return visit(pin)
	})
}

func readPinnedCustody(root string) (marker, error) {
	dir, err := safeopen.Open(root)
	if err != nil {
		return marker{}, err
	}
	defer func() { _ = dir.Close() }()
	f, err := safeopen.OpenAt(dir, pinnedCustodyFile)
	if err != nil {
		return marker{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return marker{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > 8192 {
		return marker{}, fmt.Errorf("invalid pinned custody file")
	}
	data, err := io.ReadAll(io.LimitReader(f, 8193))
	if err != nil {
		return marker{}, err
	}
	if len(data) > 8192 {
		return marker{}, fmt.Errorf("pinned custody exceeds byte limit")
	}
	var owner marker
	if err := json.Unmarshal(data, &owner); err != nil {
		return marker{}, err
	}
	if !validRunID(owner.OwnerRunID) || owner.RunID != owner.OwnerRunID || owner.RepositoryDigest == "" || owner.CreatedAt.IsZero() {
		return marker{}, fmt.Errorf("invalid pinned custody identity")
	}
	return owner, nil
}

func (m *Manager) recordPinnedCustody(ctx context.Context, key string, opts PinnedOptions) error {
	// An operator reset creates no implementation run. Keep the prior identity
	// until a real run takes custody; its preserved archive is independent.
	if opts.RunID == "workspace-reset" {
		return nil
	}
	baseRef := pinnedBaseRef(ctx, filepath.Join(m.pinnedRoot, key, "pin"), opts.BaseRef)
	return writeMarker(filepath.Join(m.pinnedRoot, key, pinnedCustodyFile), marker{
		RunID: opts.RunID, OwnerRunID: opts.RunID,
		BaseRef: baseRef, RepositoryDigest: RepositoryDigest(opts.RepoURL), CreatedAt: time.Now().UTC(),
	})
}
