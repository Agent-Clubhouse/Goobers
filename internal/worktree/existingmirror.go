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
	if rooted, err := managerRootDirectory(m.Root); err != nil || !rooted {
		return false, err
	}
	// Do not follow substituted repository directories into operator-owned
	// repositories. These directories are this manager's own, created and
	// removed by path beneath Root, so a symlink here is a substitution.
	for _, path := range []string{filepath.Join(m.Root, key), dir} {
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

// managerRootDirectory resolves the manager's configured root through an
// alias (os.Stat, not os.Lstat) and reports whether a directory is there at
// all. Root is the location this manager was given, not one it substitutes:
// an instance migrated from the pre-gaggle layout keeps the instance root's
// workcopies entry as a symlink to the gaggle's own directory, and a
// pinned-project gaggle roots its manager exactly there. Judging that alias by
// the rule meant for the mirror directories beneath it refused every existing
// mirror visit on such an instance, so the retention sweep, the eviction hook
// and custody prune could not retire a single snapshot (the pinned-root
// sibling of this check was fixed the same way). A root that is present but is
// not a directory is still a refusal: nothing legitimate puts a file there.
func managerRootDirectory(root string) (bool, error) {
	info, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect existing managed mirror root: %w", err)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("existing managed mirror root must be a directory")
	}
	return true, nil
}
