package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// CleanPolicy controls which non-versioned files survive between pinned runs.
type CleanPolicy string

const (
	CleanNone        CleanPolicy = "none"
	CleanIgnoredSafe CleanPolicy = "ignored-safe"
	CleanFull        CleanPolicy = "full"
)

type pinnedWaiter struct {
	ready chan struct{}
}

type pinnedLeaseState struct {
	mu      sync.Mutex
	held    bool
	waiters []*pinnedWaiter
}

var pinnedLeases sync.Map

// AcquirePinnedLease serializes complete runs targeting one pinned repository.
// onQueued receives the stable 1-based position observed when the run joins.
func (m *Manager) AcquirePinnedLease(ctx context.Context, repoURL, runID string, onQueued func(int)) (func() error, error) {
	key := repoKey(repoURL)
	stateKey := m.Root + "\x00" + key
	value, _ := pinnedLeases.LoadOrStore(stateKey, &pinnedLeaseState{})
	state := value.(*pinnedLeaseState)

	state.mu.Lock()
	if !state.held {
		state.held = true
		state.mu.Unlock()
		return m.persistPinnedLease(key, runID, func() { releasePinnedState(state) })
	}
	waiter := &pinnedWaiter{ready: make(chan struct{})}
	state.waiters = append(state.waiters, waiter)
	position := len(state.waiters)
	state.mu.Unlock()
	if onQueued != nil {
		onQueued(position)
	}

	select {
	case <-ctx.Done():
		state.mu.Lock()
		found := false
		for i, queued := range state.waiters {
			if queued == waiter {
				state.waiters = append(state.waiters[:i], state.waiters[i+1:]...)
				found = true
				break
			}
		}
		state.mu.Unlock()
		if found {
			return nil, ctx.Err()
		}
		// Release already handed this waiter ownership while cancellation
		// raced the ready signal; finish acquisition so ownership is not lost.
		return m.persistPinnedLease(key, runID, func() { releasePinnedState(state) })
	case <-waiter.ready:
		return m.persistPinnedLease(key, runID, func() { releasePinnedState(state) })
	}
}

func releasePinnedState(state *pinnedLeaseState) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.waiters) == 0 {
		state.held = false
		return
	}
	next := state.waiters[0]
	state.waiters = state.waiters[1:]
	close(next.ready)
}

func (m *Manager) persistPinnedLease(key, runID string, releaseState func()) (func() error, error) {
	path := filepath.Join(m.Root, key, "pin.lease")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		releaseState()
		return nil, fmt.Errorf("worktree: create pinned lease directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		releaseState()
		if os.IsExist(err) {
			return nil, fmt.Errorf("worktree: pinned workspace has a stale or externally held lease; explicit workspace reset is required")
		}
		return nil, fmt.Errorf("worktree: acquire pinned lease: %w", err)
	}
	if _, err := file.WriteString(runID + "\n"); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		releaseState()
		return nil, fmt.Errorf("worktree: persist pinned lease: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		releaseState()
		return nil, fmt.Errorf("worktree: close pinned lease: %w", err)
	}
	return func() error {
		err := os.Remove(path)
		releaseState()
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("worktree: release pinned lease: %w", err)
		}
		return nil
	}, nil
}

// PreparePinned refreshes and resets the stable checkout for one leased run.
func (m *Manager) PreparePinned(ctx context.Context, repoURL, runID, baseRef, branch string, syncBase bool, policy CleanPolicy) (*Worktree, error) {
	if baseRef == "" {
		return nil, fmt.Errorf("worktree: pinned BaseRef is required")
	}
	repoDir, err := m.workingCopy(ctx, repoURL, true)
	if err != nil {
		return nil, err
	}
	key := repoKey(repoURL)
	path := m.pinDirForKey(key)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := runGit(ctx, "", "clone", "--no-checkout", repoDir, path); err != nil {
			return nil, fmt.Errorf("worktree: create pinned workspace: %w", err)
		}
		if err := runGit(ctx, path, "remote", "rename", "origin", "mirror"); err != nil {
			return nil, fmt.Errorf("worktree: configure pinned mirror: %w", err)
		}
		if err := runGit(ctx, path, "remote", "add", "origin", repoURL); err != nil {
			return nil, fmt.Errorf("worktree: configure pinned origin: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("worktree: stat pinned workspace: %w", err)
	}
	if err := runGit(ctx, path, "fetch", "--prune", "mirror",
		"+refs/heads/*:refs/remotes/mirror/*", "+refs/tags/*:refs/tags/*"); err != nil {
		return nil, fmt.Errorf("worktree: refresh pinned workspace: %w", err)
	}
	base := baseRef
	if refExists(ctx, path, "refs/remotes/mirror/"+baseRef) {
		base = "refs/remotes/mirror/" + baseRef
		if err := runGit(ctx, path, "update-ref", "refs/heads/"+baseRef, base); err != nil {
			return nil, fmt.Errorf("worktree: update pinned base branch %q: %w", baseRef, err)
		}
	}
	if branch != "" && branchExists(ctx, path, branch) {
		if err := runGit(ctx, path, "checkout", "-f", branch); err != nil {
			return nil, fmt.Errorf("worktree: checkout pinned branch %q: %w", branch, err)
		}

		if err := runGit(ctx, path, "reset", "--hard", branch); err != nil {
			return nil, fmt.Errorf("worktree: reset pinned branch %q: %w", branch, err)
		}
		if syncBase {
			if err := runGit(ctx, path, "merge", "--ff", "--no-edit", base); err != nil {
				return nil, fmt.Errorf("worktree: sync pinned branch %q with %q: %w", branch, baseRef, err)
			}
		}
	} else if branch != "" {
		if err := runGit(ctx, path, "checkout", "-B", branch, base); err != nil {
			return nil, fmt.Errorf("worktree: create pinned branch %q: %w", branch, err)
		}
	} else {
		if err := runGit(ctx, path, "checkout", "--detach", "-f", base); err != nil {
			return nil, fmt.Errorf("worktree: checkout pinned base %q: %w", baseRef, err)
		}
	}
	switch policy {
	case "", CleanNone:
	case CleanIgnoredSafe:
		err = runGit(ctx, path, "clean", "-ffd")
	case CleanFull:
		err = runGit(ctx, path, "clean", "-ffdx")
	default:
		return nil, fmt.Errorf("worktree: unknown pinned clean policy %q", policy)
	}
	if err != nil {
		return nil, fmt.Errorf("worktree: clean pinned workspace: %w", err)
	}
	if err := runGit(ctx, path, "config", "user.name", botGitUserName); err != nil {
		return nil, err
	}
	if err := runGit(ctx, path, "config", "user.email", botGitUserEmail); err != nil {
		return nil, err
	}
	startRef, err := gitOutput(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	return &Worktree{
		RunID: runID, Path: path, Branch: branch, manager: m, key: key,
		startRef: startRef, repoURL: repoURL, pinned: true,
	}, nil
}

func refExists(ctx context.Context, repoDir, ref string) bool {
	return runGit(ctx, repoDir, "rev-parse", "--verify", "--quiet", ref) == nil
}

// SwitchPinnedBranch rebinds a leased run to an existing local branch.
func (wt *Worktree) SwitchPinnedBranch(ctx context.Context, branch string) error {
	if !wt.pinned {
		return fmt.Errorf("worktree: branch switching is only valid for pinned workspaces")
	}
	if !branchExists(ctx, wt.Path, branch) {
		return fmt.Errorf("worktree: pinned branch %q does not exist (refusing to create it)", branch)
	}
	if err := runGit(ctx, wt.Path, "checkout", "-f", branch); err != nil {
		return fmt.Errorf("worktree: switch pinned branch to %q: %w", branch, err)
	}
	startRef, err := gitOutput(ctx, wt.Path, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("worktree: resolve pinned branch %q: %w", branch, err)
	}
	wt.Branch = branch
	wt.startRef = startRef
	return nil
}

// SyncPinnedBase merges the already-refreshed local base into the run branch.
func (wt *Worktree) SyncPinnedBase(ctx context.Context, baseRef string) error {
	if !wt.pinned {
		return fmt.Errorf("worktree: base sync is only valid for pinned workspaces")
	}
	if err := runGit(ctx, wt.Path, "merge", "--ff", "--no-edit", baseRef); err != nil {
		return fmt.Errorf("worktree: sync pinned branch %q with %q: %w", wt.Branch, baseRef, err)
	}
	return nil
}
