package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// CreateOptions configures a single per-run worktree.
type CreateOptions struct {
	// RepoURL identifies the target repo; fed to Manager.WorkingCopy.
	RepoURL string
	// RunID uniquely identifies this run. It keys the worktree's path and
	// marker, so it must be unique per Manager for the lifetime of the run.
	RunID string
	// BaseRef is the pinned ref (branch, tag, or commit sha) to branch or
	// check out from. Required.
	BaseRef string
	// Branch, if set, is created and checked out from BaseRef (e.g.
	// "goobers/<workflow>/<run-id>"). If empty, the worktree is a detached
	// checkout of BaseRef.
	Branch string
}

// Worktree is a disposable, isolated working copy for one run, branched off
// a Manager's managed working copy. Obtain one via Manager.Create and release
// it via Remove.
type Worktree struct {
	// RunID is the run this worktree was created for.
	RunID string
	// Path is the worktree's filesystem location — hand this to the stage.
	Path string
	// Branch is the branch checked out in the worktree, or empty if detached.
	Branch string

	manager *Manager
	key     string
}

// Create prepares repoURL's managed working copy (cloning or fetching as
// needed) and adds a new worktree off it for opts.BaseRef, keyed by
// opts.RunID. Two calls with different RunIDs against the same repo may run
// concurrently and never observe each other's worktree contents.
func (m *Manager) Create(ctx context.Context, opts CreateOptions) (*Worktree, error) {
	if opts.RunID == "" {
		return nil, fmt.Errorf("worktree: RunID is required")
	}
	if opts.BaseRef == "" {
		return nil, fmt.Errorf("worktree: BaseRef is required")
	}

	repoDir, err := m.WorkingCopy(ctx, opts.RepoURL)
	if err != nil {
		return nil, err
	}
	key := repoKey(opts.RepoURL)

	// Worktree add mutates the repo's administrative worktree list; serialize
	// it per repo alongside clone/fetch so concurrent Creates for the same
	// repo don't race git's internal locking.
	lock := m.lockFor(key)
	lock.Lock()
	defer lock.Unlock()

	path := filepath.Join(m.runsDirForKey(key), opts.RunID)
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("worktree: run %s already has a worktree at %s", opts.RunID, path)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("worktree: stat %s: %w", path, err)
	}
	if err := os.MkdirAll(m.runsDirForKey(key), 0o755); err != nil {
		return nil, fmt.Errorf("worktree: create runs dir: %w", err)
	}

	args := []string{"worktree", "add"}
	if opts.Branch != "" {
		args = append(args, "-b", opts.Branch)
	} else {
		args = append(args, "--detach")
	}
	args = append(args, path, opts.BaseRef)
	if err := runGit(ctx, repoDir, args...); err != nil {
		return nil, fmt.Errorf("worktree: create for run %s: %w", opts.RunID, err)
	}

	mk := marker{RunID: opts.RunID, PID: os.Getpid(), CreatedAt: time.Now(), Status: statusActive}
	if err := writeMarker(m.markerPath(key, opts.RunID), mk); err != nil {
		// Without a marker, Reap can never distinguish this worktree from an
		// orphan, so don't leave it behind half-registered.
		_ = runGit(ctx, repoDir, "worktree", "remove", "--force", path)
		return nil, fmt.Errorf("worktree: register run %s: %w", opts.RunID, err)
	}

	return &Worktree{RunID: opts.RunID, Path: path, Branch: opts.Branch, manager: m, key: key}, nil
}

// RemoveOptions configures worktree teardown.
type RemoveOptions struct {
	// Keep leaves the worktree on disk for debugging instead of removing it
	// (the run's declared keep-on-failure policy). A kept worktree is only
	// swept up later by Reap, once it ages past ReapOptions.StaleAfter.
	Keep bool
}

// Remove tears down the worktree: by default it removes the worktree from
// disk and unregisters it; with RemoveOptions.Keep it leaves the worktree in
// place and marks it kept, so Reap does not treat it as a crash orphan.
func (wt *Worktree) Remove(ctx context.Context, opts RemoveOptions) error {
	repoDir := wt.manager.repoDirForKey(wt.key)
	markerPath := wt.manager.markerPath(wt.key, wt.RunID)

	if opts.Keep {
		mk, err := readMarker(markerPath)
		if err != nil {
			return fmt.Errorf("worktree: read marker for run %s: %w", wt.RunID, err)
		}
		mk.Status = statusKept
		if err := writeMarker(markerPath, mk); err != nil {
			return fmt.Errorf("worktree: mark run %s kept: %w", wt.RunID, err)
		}
		return nil
	}

	lock := wt.manager.lockFor(wt.key)
	lock.Lock()
	defer lock.Unlock()

	if err := runGit(ctx, repoDir, "worktree", "remove", "--force", wt.Path); err != nil {
		return fmt.Errorf("worktree: remove for run %s: %w", wt.RunID, err)
	}
	if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("worktree: unregister run %s: %w", wt.RunID, err)
	}
	return nil
}
