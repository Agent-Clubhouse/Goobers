package worktree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// Manager owns managed working copies under Root — one mirror clone per
// distinct repo URL — and hands out per-run worktrees branched off them. The
// zero value is not usable; construct with NewManager.
type Manager struct {
	// Root is the workcopies directory (ARCHITECTURE.md §6:
	// <instance-root>/workcopies).
	Root string

	mu        sync.Mutex // guards repoLocks
	repoLocks map[string]*sync.Mutex
}

// NewManager returns a Manager rooted at root, creating the directory if it
// does not already exist.
func NewManager(root string) (*Manager, error) {
	if root == "" {
		return nil, fmt.Errorf("worktree: root must not be empty")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("worktree: create root %s: %w", root, err)
	}
	return &Manager{Root: root, repoLocks: make(map[string]*sync.Mutex)}, nil
}

// repoKey derives a stable, filesystem-safe directory name for a repo URL so
// two managers (or two runs) referring to the same repo always land on the
// same managed working copy.
func repoKey(repoURL string) string {
	sum := sha256.Sum256([]byte(repoURL))
	return hex.EncodeToString(sum[:])[:16]
}

func (m *Manager) repoDirForKey(key string) string {
	return filepath.Join(m.Root, key, "repo.git")
}

func (m *Manager) runsDirForKey(key string) string {
	return filepath.Join(m.Root, key, "runs")
}

func (m *Manager) markersDirForKey(key string) string {
	return filepath.Join(m.Root, key, "markers")
}

func (m *Manager) markerPath(key, runID string) string {
	return filepath.Join(m.markersDirForKey(key), runID+".json")
}

// lockFor returns the per-repo mutex used to serialize clone/fetch and
// worktree-add for a given repo, creating it on first use.
func (m *Manager) lockFor(key string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.repoLocks[key]
	if !ok {
		l = &sync.Mutex{}
		m.repoLocks[key] = l
	}
	return l
}

// WorkingCopy ensures a managed mirror clone of repoURL exists and is up to
// date under Root, cloning on first use and fetching thereafter. A mirror
// clone has no working tree of its own — worktrees created via Create are the
// only mutable views onto it — and its fetch refspec covers every ref, so a
// pinned base ref (branch, tag, or sha) reachable on the remote is always
// available to branch a worktree from after WorkingCopy returns.
//
// Concurrent calls for the same repo URL serialize on the clone/fetch step;
// calls for different repos proceed independently.
func (m *Manager) WorkingCopy(ctx context.Context, repoURL string) (string, error) {
	key := repoKey(repoURL)
	lock := m.lockFor(key)
	lock.Lock()
	defer lock.Unlock()

	dir := m.repoDirForKey(key)
	switch _, err := os.Stat(dir); {
	case os.IsNotExist(err):
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", fmt.Errorf("worktree: create workcopy parent for %s: %w", repoURL, err)
		}
		if err := runGit(ctx, "", "clone", "--mirror", repoURL, dir); err != nil {
			_ = os.RemoveAll(dir) // don't leave a partial clone masquerading as a valid one
			return "", fmt.Errorf("worktree: clone %s: %w", repoURL, err)
		}
		return dir, nil
	case err != nil:
		return "", fmt.Errorf("worktree: stat workcopy for %s: %w", repoURL, err)
	}

	if err := runGit(ctx, dir, "fetch", "--prune", "origin"); err != nil {
		return "", fmt.Errorf("worktree: fetch %s: %w", repoURL, err)
	}
	return dir, nil
}

// runGit runs git with args, using dir as the working directory (the process
// default if dir is empty), and returns combined output on failure for
// debuggability.
func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %v: %w: %s", args, err, out)
	}
	return nil
}
