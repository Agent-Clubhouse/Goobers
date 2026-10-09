package worktree

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

// An interrupted add/remove may leave an empty directory. Remove only that
// empty leaf after proving archive ownership; Remove refuses any file which
// appears meanwhile. A nonempty directory always takes the preserving path.
func (m *Manager) removeEmptyParentRestore(ctx context.Context, key, repository, path string, opts CreateOptions) (bool, error) {
	dir, err := os.Open(path)
	if err != nil {
		return false, err
	}
	entries, readErr := dir.ReadDir(1)
	closeErr := dir.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return false, errors.Join(readErr, closeErr)
	}
	if len(entries) != 0 || closeErr != nil {
		return false, closeErr
	}
	wt := &Worktree{RunID: opts.RunID, Path: path, Branch: opts.Branch, manager: m, key: key, repoURL: opts.RepoURL}
	mk, _, err := wt.parentRestoreMarkers(opts)
	if err != nil {
		return true, err
	}
	if err := validateParentRestoreMarker(ctx, repository, mk, opts); err != nil {
		return true, err
	}
	if err := ensureParentRestoreBranch(ctx, repository, opts); err != nil {
		return true, err
	}
	if err := validateEmptyParentRegistration(ctx, repository, path, opts.Branch); err != nil {
		return true, err
	}
	return true, os.Remove(path)
}

func validateEmptyParentRegistration(ctx context.Context, repository, path, branch string) error {
	entries, err := registeredWorktrees(ctx, repository)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if sameWorktreePath(entry.Path, path) && entry.Branch != "refs/heads/"+branch {
			return errors.New("parent restore empty directory has another registered branch")
		}
	}
	return nil
}

// Called under the repository lock, after the exact target was found absent.
// Surviving ownership can precede a failed worktree add or follow an interrupted
// removal. Validate it against the archive before replacing its metadata.
func (m *Manager) prepareAbsentParentRestore(ctx context.Context, key, repository, path string, opts CreateOptions) error {
	owned := false
	for _, markerPath := range []string{m.markerPath(key, opts.RunID), m.ownershipPath(key, worktreeDirectoryName(opts.RunID))} {
		if _, err := os.Lstat(markerPath); err == nil {
			owned = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if owned {
		wt := &Worktree{RunID: opts.RunID, Path: path, Branch: opts.Branch, manager: m, key: key, repoURL: opts.RepoURL}
		mk, _, err := wt.parentRestoreMarkers(opts)
		if err != nil {
			return err
		}
		if err := validateParentRestoreMarker(ctx, repository, mk, opts); err != nil {
			return err
		}
	}
	if err := ensureParentRestoreBranch(ctx, repository, opts); err != nil {
		return err
	}
	return clearAbsentParentRegistration(ctx, repository, path, opts.Branch, owned)
}

func validateParentRestoreMarker(ctx context.Context, repository string, mk marker, opts CreateOptions) error {
	if mk.OwnerRunID != opts.OwnerRunID || mk.Gaggle != opts.Gaggle || mk.StartRef != opts.parentRestoreStart || mk.BaseRef != resolvedCleanupBaseRef(ctx, repository, opts.BaseRef) || !mk.RetainOnCleanup {
		return fmt.Errorf("parent restore workspace identity changed")
	}
	return nil
}

func clearAbsentParentRegistration(ctx context.Context, repository, path, branch string, owned bool) error {
	entries, err := registeredWorktrees(ctx, repository)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !sameWorktreePath(entry.Path, path) {
			continue
		}
		if !owned || entry.Branch != "refs/heads/"+branch || !entry.Prunable {
			return errors.New("parent restore refuses unrelated or live registration")
		}
		// Remove only this absent checkout's stale Git administrative entry,
		// never sweep other workspaces or delete a branch. Its archived HEAD
		// was verified above, and its durable ownership records remain until
		// normal creation replaces them.
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return errors.New("parent restore target reappeared before registration cleanup")
		}
		return runGit(ctx, repository, "worktree", "remove", "--force", path)
	}
	return nil
}
