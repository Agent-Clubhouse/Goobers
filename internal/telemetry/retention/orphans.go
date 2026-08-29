package retention

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

// MinimumOrphanAge is the non-reducible safety window for orphan cleanup.
const MinimumOrphanAge = 24 * time.Hour

const orphanStagingDirName = ".orphan-pruning"

type orphanRoot struct {
	path          string
	pruningPath   string
	creationStage bool
}

// OrphanOptions controls one explicit orphan cleanup operation.
type OrphanOptions struct {
	Now    time.Time
	MinAge time.Duration
	Delete bool
}

// OrphanResult describes an orphan selected or deleted by cleanup.
type OrphanResult struct {
	Name          string
	RunDir        string
	LastModified  time.Time
	CreationStage bool

	root orphanRoot
}

// PruneOrphans reports or deletes old directories that are orphaned runs or
// unpublished creation-stage residue.
func PruneOrphans(layout instance.Layout, opts OrphanOptions) ([]OrphanResult, error) {
	if opts.MinAge < MinimumOrphanAge {
		return nil, fmt.Errorf("orphan minimum age must be at least %s", MinimumOrphanAge)
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	runRoots, err := layout.RunDirs()
	if err != nil {
		return nil, err
	}
	if opts.Delete {
		maintenanceLocks, err := journal.AcquireRunRootMaintenanceLocks(runRoots)
		if err != nil {
			return nil, err
		}
		defer func() { _ = maintenanceLocks.Release() }()
	}

	roots := orphanRoots(runRoots)
	cutoff := opts.Now.Add(-opts.MinAge)
	var deleted []OrphanResult
	if opts.Delete {
		deleted, err = finishInterruptedOrphanPrunes(roots, cutoff)
		if err != nil {
			return nil, err
		}
	}
	candidates, err := discoverOrphans(roots, cutoff)
	if err != nil || !opts.Delete {
		return candidates, err
	}

	for _, candidate := range candidates {
		removed, err := deleteOrphan(candidate, cutoff)
		if err != nil {
			return deleted, err
		}
		if removed {
			deleted = append(deleted, candidate)
		}
	}
	return deleted, nil
}

func orphanRoots(runRoots []string) []orphanRoot {
	roots := make([]orphanRoot, 0, len(runRoots)*2)
	for _, runRoot := range runRoots {
		roots = append(roots,
			orphanRoot{
				path:        runRoot,
				pruningPath: orphanStagingRoot(runRoot),
			},
			orphanRoot{
				path:          runCreationRoot(runRoot),
				pruningPath:   runCreationPruningRoot(runRoot),
				creationStage: true,
			},
		)
	}
	return roots
}

func discoverOrphans(roots []orphanRoot, cutoff time.Time) ([]OrphanResult, error) {
	var candidates []OrphanResult
	for _, root := range roots {
		entries, err := os.ReadDir(root.path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("orphan cleanup: read %s: %w", root.path, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			candidate, eligible, err := inspectOrphan(root, filepath.Join(root.path, entry.Name()), cutoff)
			if err != nil {
				return nil, err
			}
			if eligible {
				candidates = append(candidates, candidate)
			}
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].RunDir < candidates[j].RunDir
	})
	return candidates, nil
}

func inspectOrphan(root orphanRoot, dir string, cutoff time.Time) (OrphanResult, bool, error) {
	if _, err := os.Lstat(filepath.Join(dir, "run.yaml")); err == nil {
		return OrphanResult{}, false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return OrphanResult{}, false, fmt.Errorf("orphan cleanup: inspect %s: %w", dir, err)
	}
	if _, err := journal.OpenRead(dir); err == nil {
		return OrphanResult{}, false, nil
	} else if !errors.Is(err, journal.ErrNotRunDirectory) &&
		!errors.Is(err, fs.ErrNotExist) {
		return OrphanResult{}, false, fmt.Errorf("orphan cleanup: inspect %s: %w", dir, err)
	}
	lastModified, err := latestModification(dir)
	if err != nil {
		return OrphanResult{}, false, err
	}
	if lastModified.After(cutoff) {
		return OrphanResult{}, false, nil
	}
	active, err := orphanLockHeld(dir)
	if err != nil {
		return OrphanResult{}, false, err
	}
	if active {
		return OrphanResult{}, false, nil
	}
	return OrphanResult{
		Name:          filepath.Base(dir),
		RunDir:        dir,
		LastModified:  lastModified,
		CreationStage: root.creationStage,
		root:          root,
	}, true, nil
}

func orphanLockHeld(dir string) (bool, error) {
	lockPath := filepath.Join(dir, ".lock")
	if _, err := os.Lstat(lockPath); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("orphan cleanup: inspect lock %s: %w", lockPath, err)
	}
	lock, err := platformlock.TryAcquire(lockPath)
	if errors.Is(err, platformlock.ErrHeld) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("orphan cleanup: acquire lock %s: %w", lockPath, err)
	}
	if err := lock.Release(); err != nil {
		return false, fmt.Errorf("orphan cleanup: release lock %s: %w", lockPath, err)
	}
	return false, nil
}

func latestModification(dir string) (time.Time, error) {
	var latest time.Time
	err := filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(latest) {
			latest = info.ModTime()
		}
		return nil
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("orphan cleanup: inspect activity under %s: %w", dir, err)
	}
	return latest, nil
}

func deleteOrphan(candidate OrphanResult, cutoff time.Time) (bool, error) {
	root := candidate.root
	stagingRoot := root.pruningPath
	if err := os.MkdirAll(stagingRoot, 0o755); err != nil {
		return false, fmt.Errorf("orphan cleanup: create staging directory: %w", err)
	}
	staged := filepath.Join(stagingRoot, candidate.Name)
	if _, err := os.Lstat(staged); err == nil {
		stagedCandidate, eligible, inspectErr := inspectOrphan(root, staged, cutoff)
		if inspectErr != nil {
			return false, inspectErr
		}
		if !eligible {
			return false, fmt.Errorf("orphan cleanup: staged path %s is no longer an old orphan", staged)
		}
		if err := os.RemoveAll(stagedCandidate.RunDir); err != nil {
			return false, fmt.Errorf("orphan cleanup: finish staged deletion %s: %w", staged, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("orphan cleanup: inspect staging path %s: %w", staged, err)
	}

	if err := os.Rename(candidate.RunDir, staged); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("orphan cleanup: stage %s: %w", candidate.RunDir, err)
	}
	stagedCandidate, eligible, err := inspectOrphan(root, staged, cutoff)
	if err != nil {
		return false, errors.Join(err, os.Rename(staged, candidate.RunDir))
	}
	if !eligible {
		if err := os.Rename(staged, candidate.RunDir); err != nil {
			return false, fmt.Errorf("orphan cleanup: restore ineligible directory %s: %w", candidate.RunDir, err)
		}
		return false, nil
	}
	if err := os.RemoveAll(stagedCandidate.RunDir); err != nil {
		return false, fmt.Errorf("orphan cleanup: remove %s: %w", candidate.RunDir, err)
	}
	return true, nil
}

func finishInterruptedOrphanPrunes(roots []orphanRoot, cutoff time.Time) ([]OrphanResult, error) {
	var deleted []OrphanResult
	for _, root := range roots {
		stagingRoot := root.pruningPath
		entries, err := os.ReadDir(stagingRoot)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("orphan cleanup: read staging directory %s: %w", stagingRoot, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			staged := filepath.Join(stagingRoot, entry.Name())
			candidate, eligible, err := inspectOrphan(root, staged, cutoff)
			if err != nil {
				return nil, err
			}
			original := filepath.Join(root.path, entry.Name())
			if !eligible {
				if _, err := os.Lstat(original); err == nil {
					return nil, fmt.Errorf("orphan cleanup: cannot restore %s because %s exists", staged, original)
				} else if !errors.Is(err, fs.ErrNotExist) {
					return nil, fmt.Errorf("orphan cleanup: inspect restore path %s: %w", original, err)
				}
				if err := os.Rename(staged, original); err != nil {
					return nil, fmt.Errorf("orphan cleanup: restore interrupted prune %s: %w", original, err)
				}
				continue
			}
			if err := os.RemoveAll(staged); err != nil {
				return nil, fmt.Errorf("orphan cleanup: finish interrupted prune %s: %w", original, err)
			}
			candidate.RunDir = original
			deleted = append(deleted, candidate)
		}
	}
	sort.Slice(deleted, func(i, j int) bool {
		return deleted[i].RunDir < deleted[j].RunDir
	})
	return deleted, nil
}

func orphanStagingRoot(runRoot string) string {
	return filepath.Join(filepath.Dir(runRoot), orphanStagingDirName)
}

func runCreationRoot(runRoot string) string {
	return journal.RunCreationStagingDir(runRoot)
}

func runCreationPruningRoot(runRoot string) string {
	return journal.RunCreationStagingDir(runRoot) + "-orphan-pruning"
}
