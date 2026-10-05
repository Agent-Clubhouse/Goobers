package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AssertRunReleased refuses to recreate a run from a remote branch while local
// workspace custody remains. It does not remove, reset or silently omit held
// files. The caller must separately prove source execution has settled and
// verify the published branch and retained recovery inventory. Adopting a held
// workspace requires a separate explicit custody transfer.
func (m *Manager) AssertRunReleased(ctx context.Context, runID string) error {
	if m == nil || !validRunID(runID) {
		return errors.New("worktree: restart requires a valid manager and source run")
	}
	keys, err := managedRepositoryKeys(m.Root)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if err := m.assertRepoRunReleased(ctx, key, runID); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) assertRepoRunReleased(ctx context.Context, key, runID string) error {
	lock := m.lockFor(key)
	if !lock.TryLock() {
		return errors.New("worktree: restart source inspection is busy; retry after repository activity settles")
	}
	defer lock.Unlock()
	for _, directory := range []string{m.markersDirForKey(key), filepath.Join(m.Root, key, "owners")} {
		entries, err := os.ReadDir(directory)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("worktree: inspect source custody: %w", err)
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			mk, err := readMarker(filepath.Join(directory, entry.Name()))
			if err != nil || !validRunID(mk.RunID) {
				return errors.New("worktree: restart cannot attribute unreadable workspace custody")
			}
			if ownedByRun(mk, mk.RunID, runID) {
				return fmt.Errorf("worktree: source run %q still holds a workspace; restart requires explicit retained-workspace adoption", runID)
			}
		}
	}
	// A checkout without its ownership record cannot be attributed safely.
	entries, err := os.ReadDir(m.runsDirForKey(key))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.IsDir() {
			return errors.New("worktree: restart found an unrecognized workspace entry")
		}
		mk, err := readMarker(m.ownershipPath(key, entry.Name()))
		if err != nil || !validRunID(mk.RunID) || mk.Directory != entry.Name() {
			return errors.New("worktree: restart cannot attribute an unowned checkout")
		}
	}
	return ctx.Err()
}
