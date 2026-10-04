package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// WithUnoccupiedBranch excludes this manager's checkout creation/recovery while
// a host applies an expected-head provider operation. It never clones/fetches or
// obtains credentials. Missing mirrors still acquire the repository mutex.
// Callers must also exclude new run/claim owners and independent process writers;
// this method is not a cross-instance lease. The callback must join bounded work.
func (m *Manager) WithUnoccupiedBranch(ctx context.Context, repoURL, branch string, use func(context.Context) error) error {
	if m == nil || repoURL == "" || branch == "" || strings.HasPrefix(branch, "refs/") || use == nil {
		return errors.New("worktree: exact unoccupied branch custody required")
	}
	key := repoKey(repoURL)
	lock := m.lockFor(key)
	for !lock.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	// Pinned workspaces have a distinct whole-run lease and are not admitted by
	// this per-stage branch guard, including retained or ambiguous old pins.
	for _, name := range []string{"pin", "lease.queue", "lease.json"} {
		_, err := os.Lstat(filepath.Join(m.pinnedRoot, key, name))
		if err == nil {
			return errors.New("worktree: pinned repository custody requires separate settlement")
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	occupied, err := m.BranchOccupancies(ctx, repoURL)
	if err != nil {
		return err
	}
	if _, found := occupied[branch]; found {
		return errors.New("worktree: selected branch still has managed custody")
	}
	return use(ctx)
}
