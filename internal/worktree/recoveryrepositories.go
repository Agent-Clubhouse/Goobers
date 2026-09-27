package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/platform/lock"
)

// WithRecoveryRepositories visits the existing mirror and pinned clone together
// so retirement can unpin every copy before removing its archive. A pinned
// clone is visited only under its whole-run lease lock. Busy leases defer the
// entire operation; the visitor never runs on a partial repository set.
func (m *Manager) WithRecoveryRepositories(ctx context.Context, repoURL string, visit func([]string) error) (bool, error) {
	if visit == nil {
		return false, fmt.Errorf("recovery repositories require visitor")
	}
	key := repoKey(repoURL)
	lock := m.lockFor(key)
	lock.Lock()
	defer lock.Unlock()
	return m.withRecoveryRepositoriesLocked(ctx, key, repoURL, visit)
}

// WithRecoveryRepositoriesLocked behaves like WithRecoveryRepositories, for a
// caller that already holds this manager's lock for repoURL's repository key
// — specifically, a cleanup guard callback, which the worktree teardown path
// (worktree.go) always invokes with that lock already held (#4823). Calling
// WithRecoveryRepositories itself from such a callback deadlocks on Go's
// non-reentrant sync.Mutex; this lets the callback retire another entry for
// the SAME repository without releasing and re-acquiring the lock it is
// already inside. It must never be called except from inside that lock.
func (m *Manager) WithRecoveryRepositoriesLocked(ctx context.Context, repoURL string, visit func([]string) error) (bool, error) {
	if visit == nil {
		return false, fmt.Errorf("recovery repositories require visitor")
	}
	return m.withRecoveryRepositoriesLocked(ctx, repoKey(repoURL), repoURL, visit)
}

// TryWithRecoveryRepositories behaves like WithRecoveryRepositories but never
// waits for this manager's repository lock. It exists for a caller that is
// already inside ANOTHER repository's lock — the capacity-pressure eviction
// hook running inside a cleanup guard — where blocking on a second repository
// lock lets two concurrent cleanups in opposite repositories wait on each
// other forever. A contended repository is simply not visited (found is
// false, error nil), which for eviction means "try a different candidate",
// never "fail the cleanup".
func (m *Manager) TryWithRecoveryRepositories(ctx context.Context, repoURL string, visit func([]string) error) (bool, error) {
	if visit == nil {
		return false, fmt.Errorf("recovery repositories require visitor")
	}
	key := repoKey(repoURL)
	lock := m.lockFor(key)
	if !lock.TryLock() {
		return false, nil
	}
	defer lock.Unlock()
	return m.withRecoveryRepositoriesLocked(ctx, key, repoURL, visit)
}

func (m *Manager) withRecoveryRepositoriesLocked(ctx context.Context, key, repoURL string, visit func([]string) error) (bool, error) {
	var found bool
	entered, err := m.withExistingMirrorLocked(ctx, key, func(mirror string) error {
		var err error
		found, err = m.withPinnedRecoveryRepository(ctx, repoURL, []string{mirror}, visit)
		return err
	})
	if err != nil || entered {
		return found, err
	}
	return m.withPinnedRecoveryRepository(ctx, repoURL, nil, visit)
}

func (m *Manager) withPinnedRecoveryRepository(ctx context.Context, repoURL string, repositories []string, visit func([]string) error) (bool, error) {
	root := filepath.Join(m.pinnedRoot, repoKey(repoURL))
	pin := filepath.Join(root, "pin")
	exists, err := pinnedRecoveryCustodyExists(m.pinnedRoot, root, pin)
	if err != nil {
		return false, err
	}
	if exists {
		held, err := lock.TryAcquireExisting(filepath.Join(root, "pin.lock"))
		if err != nil {
			return false, fmt.Errorf("acquire pinned recovery custody: %w", err)
		}
		defer func() { _ = held.Release() }()
		if exists, err := pinnedRecoveryCustodyExists(m.pinnedRoot, root, pin); err != nil || !exists {
			return false, fmt.Errorf("pinned recovery directory changed before custody: %w", errors.Join(err, os.ErrNotExist))
		}
		repositories = append(repositories, pin)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if len(repositories) == 0 {
		return false, nil
	}
	return true, visit(repositories)
}

// pinnedRecoveryCustodyExists reports whether this repository's pinned custody
// directories are present, with pinnedRoot judged separately from the custody
// directories beneath it.
//
// pinnedRoot is the node-wide workcopies root this package is given, not one it
// creates: an instance migrated from the pre-gaggle layout keeps the instance
// root's workcopies entry as an alias to the gaggle's own directory, and that
// alias is the supported shape. Requiring it to be a real directory refused
// EVERY recovery repository visit on such an instance, so the retention sweep,
// the eviction hook and custody prune could not retire a single snapshot and
// the inventory only ever grew. The per-repository custody directories are this
// package's own and stay strict: they are unpinned and removed by path, so a
// substituted symlink there would delete through it.
func pinnedRecoveryCustodyExists(pinnedRoot, root, pin string) (bool, error) {
	rooted, err := pinnedRecoveryRootDirectory(pinnedRoot)
	if err != nil || !rooted {
		return false, err
	}
	return realPinnedRecoveryDirectory(root, pin)
}

// pinnedRecoveryRootDirectory resolves the node-wide root through an alias
// (os.Stat, not os.Lstat) and reports whether a directory is there at all. A
// root that is present but is not a directory is still a refusal: nothing
// legitimate puts a file where the workcopies root belongs.
func pinnedRecoveryRootDirectory(path string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("pinned recovery root must be a directory")
	}
	return true, nil
}

func realPinnedRecoveryDirectory(paths ...string) (bool, error) {
	for _, path := range paths {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !info.IsDir() {
			return false, fmt.Errorf("pinned recovery requires real directories")
		}
	}
	return true, nil
}
