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
	exists, err := realPinnedRecoveryDirectory(m.pinnedRoot, root, pin)
	if err != nil {
		return false, err
	}
	if exists {
		held, err := lock.TryAcquireExisting(filepath.Join(root, "pin.lock"))
		if err != nil {
			return false, fmt.Errorf("acquire pinned recovery custody: %w", err)
		}
		defer func() { _ = held.Release() }()
		if exists, err := realPinnedRecoveryDirectory(m.pinnedRoot, root, pin); err != nil || !exists {
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
