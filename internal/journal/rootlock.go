package journal

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

// RunRootMaintenanceLocks serialize operations that discover and then move run
// directories, such as telemetry rebuild and retention.
type RunRootMaintenanceLocks struct {
	handles []*platformlock.Handle
}

// AcquireRunRootMaintenanceLocks acquires one lock per run root in deterministic
// order. The lock lives beside the root so it is never mistaken for a run.
func AcquireRunRootMaintenanceLocks(runRoots []string) (*RunRootMaintenanceLocks, error) {
	paths := make([]string, 0, len(runRoots))
	seen := make(map[string]struct{}, len(runRoots))
	for _, root := range runRoots {
		root = filepath.Clean(root)
		path := filepath.Join(filepath.Dir(root), "."+filepath.Base(root)+".maintenance.lock")
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	sort.Strings(paths)

	locks := &RunRootMaintenanceLocks{}
	for _, path := range paths {
		handle, err := acquireJournalLockPath(path, path, "run-root maintenance")
		if err != nil {
			return nil, errors.Join(
				fmt.Errorf("journal: acquire run-root maintenance lock: %w", err),
				locks.Release(),
			)
		}
		locks.handles = append(locks.handles, handle)
	}
	return locks, nil
}

// Release releases every run-root maintenance lock.
func (l *RunRootMaintenanceLocks) Release() error {
	if l == nil {
		return nil
	}
	var err error
	for i := len(l.handles) - 1; i >= 0; i-- {
		err = errors.Join(err, l.handles[i].Release())
	}
	l.handles = nil
	return err
}
