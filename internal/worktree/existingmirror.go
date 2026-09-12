package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// WithExistingMirror visits the managed mirror directory for an exact clone
// URL while holding this manager's repository lock. It never creates directories,
// clones, fetches, or invokes credential resolution. Missing mirrors return false.
// The callback must validate any Git state it uses and must not call methods that
// reacquire this manager's repository lock. This is not a cross-process lifecycle
// lock or authorization to delete a run's state.
func (m *Manager) WithExistingMirror(ctx context.Context, repoURL string, visit func(string) error) (bool, error) {
	if repoURL == "" || visit == nil {
		return false, fmt.Errorf("existing mirror requires repository identity and visitor")
	}
	key := repoKey(repoURL)
	lock := m.lockFor(key)
	lock.Lock()
	defer lock.Unlock()
	return m.withExistingMirrorLocked(ctx, key, visit)
}

// withExistingMirrorLocked is WithExistingMirror's body without acquiring
// this repository's lock, for a caller that already holds it (#4823's
// WithRecoveryRepositoriesLocked, used from inside a cleanup guard, which
// already runs under this exact lock — calling WithExistingMirror itself
// there would deadlock on Go's non-reentrant sync.Mutex).
func (m *Manager) withExistingMirrorLocked(ctx context.Context, key string, visit func(string) error) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	dir := m.repoDirForKey(key)
	// Do not follow substituted repository directories into operator-owned
	// repositories. Ancestors above Root are the configured manager location.
	for _, path := range []string{m.Root, filepath.Join(m.Root, key), dir} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("inspect existing managed mirror: %w", err)
		}
		if !info.IsDir() {
			return false, fmt.Errorf("existing managed mirror requires real directories")
		}
	}
	return true, visit(dir)
}
