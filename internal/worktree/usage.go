package worktree

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// UsageOperation identifies the lifecycle boundary that produced a disk-usage
// measurement.
type UsageOperation string

const (
	// UsageOperationCreate identifies a measurement taken after worktree creation.
	UsageOperationCreate UsageOperation = "create"
	// UsageOperationTeardown identifies a measurement taken before worktree removal.
	UsageOperationTeardown UsageOperation = "teardown"
	// UsageOperationHousekeeping identifies a measurement taken during stale-worktree cleanup.
	UsageOperationHousekeeping UsageOperation = "housekeeping"
)

// UsageMeasurement reports one lifecycle disk-usage snapshot. WorkcopyBytes is
// the aggregate of managed mirrors and marker-recorded worktree snapshots.
type UsageMeasurement struct {
	Operation           UsageOperation
	Gaggle              string
	OwnerRunID          string
	WorktreeID          string
	WorktreeBytes       int64
	WorktreeMeasured    bool
	WorkcopyBytes       int64
	WorkcopyMeasured    bool
	UnmeasuredWorktrees int
	Err                 error
}

// UsageObserver receives lifecycle disk-usage measurements. Measurement is
// observational: observer failures cannot alter worktree lifecycle behavior.
type UsageObserver func(context.Context, UsageMeasurement)

// WithUsageObserver enables disk-usage measurement at create, teardown, and
// housekeeping boundaries. gaggle is included when this Manager is scoped to a
// known gaggle; legacy instance-wide managers may leave it empty.
func WithUsageObserver(gaggle string, observer UsageObserver) ManagerOption {
	return func(m *Manager) {
		m.gaggle = gaggle
		m.usageObserver = observer
	}
}

func (m *Manager) measureWorktree(path string) (int64, bool, error) {
	if m.usageObserver == nil || path == "" {
		return 0, false, nil
	}
	size, err := m.diskUsage(path)
	if err != nil {
		return 0, false, fmt.Errorf("worktree: measure %s: %w", path, err)
	}
	return size, true, nil
}

func (m *Manager) observeUsage(
	ctx context.Context,
	operation UsageOperation,
	ownerRunID, worktreeID string,
	worktreeBytes int64,
	worktreeMeasured bool,
	measurementErr error,
) {
	if m.usageObserver == nil {
		return
	}
	workcopyBytes, unmeasured, aggregateErr := m.aggregateUsage()
	m.usageObserver(ctx, UsageMeasurement{
		Operation:           operation,
		Gaggle:              m.gaggle,
		OwnerRunID:          ownerRunID,
		WorktreeID:          worktreeID,
		WorktreeBytes:       worktreeBytes,
		WorktreeMeasured:    worktreeMeasured,
		WorkcopyBytes:       workcopyBytes,
		WorkcopyMeasured:    aggregateErr == nil,
		UnmeasuredWorktrees: unmeasured,
		Err:                 errors.Join(measurementErr, aggregateErr),
	})
}

// aggregateUsage never descends into runs/: active stages may mutate those
// trees concurrently. Their last safe lifecycle measurements are persisted in
// markers and added to the managed mirror/metadata total instead.
func (m *Manager) aggregateUsage() (int64, int, error) {
	entries, err := os.ReadDir(m.Root)
	if err != nil {
		return 0, 0, fmt.Errorf("worktree: measure aggregate root %s: %w", m.Root, err)
	}

	var total int64
	var unmeasured int
	var measurementErr error
	for _, entry := range entries {
		path := filepath.Join(m.Root, entry.Name())
		if !entry.IsDir() {
			size, err := m.diskUsage(path)
			if err != nil {
				measurementErr = errors.Join(measurementErr, fmt.Errorf("worktree: measure aggregate path %s: %w", path, err))
				continue
			}
			total += size
			continue
		}
		if entry.Name() == "scratch" {
			continue
		}

		children, err := os.ReadDir(path)
		if err != nil {
			measurementErr = errors.Join(measurementErr, fmt.Errorf("worktree: list aggregate path %s: %w", path, err))
			continue
		}
		for _, child := range children {
			if child.Name() == "runs" {
				continue
			}
			childPath := filepath.Join(path, child.Name())
			size, err := m.diskUsage(childPath)
			if err != nil {
				measurementErr = errors.Join(measurementErr, fmt.Errorf("worktree: measure aggregate path %s: %w", childPath, err))
				continue
			}
			total += size
		}

		markerSizes, unknown, err := markerUsage(filepath.Join(path, "markers"))
		total += markerSizes
		unmeasured += unknown
		measurementErr = errors.Join(measurementErr, err)

		markerless, err := markerlessWorktreeCount(filepath.Join(path, "runs"), filepath.Join(path, "markers"))
		unmeasured += markerless
		measurementErr = errors.Join(measurementErr, err)
	}
	return total, unmeasured, measurementErr
}

func markerUsage(markersDir string) (int64, int, error) {
	entries, err := os.ReadDir(markersDir)
	if os.IsNotExist(err) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("worktree: list usage markers %s: %w", markersDir, err)
	}

	var total int64
	var unknown int
	var measurementErr error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(markersDir, entry.Name())
		mk, err := readMarker(path)
		if err != nil {
			unknown++
			measurementErr = errors.Join(measurementErr, fmt.Errorf("worktree: read usage marker %s: %w", path, err))
			continue
		}
		if mk.SizeBytes == nil {
			unknown++
			continue
		}
		total += *mk.SizeBytes
	}
	return total, unknown, measurementErr
}

func markerlessWorktreeCount(runsDir, markersDir string) (int, error) {
	entries, err := os.ReadDir(runsDir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("worktree: list usage runs %s: %w", runsDir, err)
	}

	var count int
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Lstat(filepath.Join(markersDir, entry.Name()+".json")); os.IsNotExist(err) {
			count++
		} else if err != nil {
			return count, fmt.Errorf("worktree: inspect usage marker for %s: %w", entry.Name(), err)
		}
	}
	return count, nil
}

// apparentDiskUsage returns the apparent bytes below root without following
// symlinks. The no-follow property keeps a repository-controlled link from
// escaping the managed workcopy root during measurement.
func apparentDiskUsage(root string) (int64, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return info.Size(), nil
	}

	var total int64
	err = filepath.WalkDir(root, func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}
