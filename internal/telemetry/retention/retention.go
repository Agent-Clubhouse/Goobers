// Package retention prunes terminal run journals and their telemetry rollups.
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
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

const stagingDirName = ".telemetry-pruning"

// Policy is the resolved retention policy. A run is eligible when either bound
// is exceeded, provided the run is terminal.
type Policy struct {
	Window  time.Duration
	MaxRuns int
}

// Options controls one prune operation.
type Options struct {
	Now    time.Time
	DryRun bool
}

// Result describes one selected or deleted run.
type Result struct {
	RunID  string
	RunDir string
	Reason string
}

type runInfo struct {
	Result
	startedAt time.Time
	terminal  bool
}

// Prune applies policy across every legacy and gaggle-scoped run root.
func Prune(layout instance.Layout, db *rollup.DB, policy Policy, opts Options) ([]Result, error) {
	if policy.Window <= 0 {
		return nil, fmt.Errorf("telemetry retention window must be positive")
	}
	if policy.MaxRuns <= 0 {
		return nil, fmt.Errorf("telemetry retention max runs must be positive")
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	runRoots, err := layout.RunDirs()
	if err != nil {
		return nil, err
	}
	if !opts.DryRun {
		if db == nil {
			return nil, fmt.Errorf("telemetry retention rollup is required")
		}
		if err := finishInterruptedPrunes(runRoots, db); err != nil {
			return nil, err
		}
	}
	runs, err := discoverRuns(runRoots)
	if err != nil {
		return nil, err
	}
	candidates := selectCandidates(runs, policy, opts.Now)
	if opts.DryRun {
		return candidates, nil
	}

	pruned := make([]Result, 0, len(candidates))
	for _, candidate := range candidates {
		deleted, err := pruneOne(candidate, db)
		if err != nil {
			return pruned, err
		}
		if deleted {
			pruned = append(pruned, candidate)
		}
	}
	return pruned, nil
}

func discoverRuns(runRoots []string) ([]runInfo, error) {
	var runs []runInfo
	seen := make(map[string]string)
	for _, root := range runRoots {
		entries, err := os.ReadDir(root)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("telemetry retention: read %s: %w", root, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			dir := filepath.Join(root, entry.Name())
			if _, err := os.Stat(filepath.Join(dir, "run.yaml")); errors.Is(err, fs.ErrNotExist) {
				continue
			} else if err != nil {
				return nil, fmt.Errorf("telemetry retention: inspect %s: %w", dir, err)
			}
			reader, err := journal.OpenRead(dir)
			if err != nil {
				return nil, err
			}
			identity, err := reader.Identity()
			if err != nil {
				return nil, err
			}
			if identity.RunID == "" || identity.StartedAt.IsZero() {
				return nil, fmt.Errorf("telemetry retention: run %s has incomplete identity", dir)
			}
			if prior, exists := seen[identity.RunID]; exists {
				return nil, fmt.Errorf("telemetry retention: run %q exists at both %s and %s", identity.RunID, prior, dir)
			}
			seen[identity.RunID] = dir
			phase, err := reader.Phase()
			if err != nil {
				return nil, fmt.Errorf("telemetry retention: read phase for run %s: %w", identity.RunID, err)
			}
			runs = append(runs, runInfo{
				Result:    Result{RunID: identity.RunID, RunDir: dir},
				startedAt: identity.StartedAt,
				terminal:  phase != journal.PhaseRunning,
			})
		}
	}
	sort.Slice(runs, func(i, j int) bool {
		if !runs[i].startedAt.Equal(runs[j].startedAt) {
			return runs[i].startedAt.After(runs[j].startedAt)
		}
		if runs[i].RunID != runs[j].RunID {
			return runs[i].RunID < runs[j].RunID
		}
		return runs[i].RunDir < runs[j].RunDir
	})
	return runs, nil
}

func selectCandidates(runs []runInfo, policy Policy, now time.Time) []Result {
	cutoff := now.Add(-policy.Window)
	var candidates []Result
	for index, run := range runs {
		if !run.terminal {
			continue
		}
		windowExceeded := !run.startedAt.After(cutoff)
		maxRunsExceeded := index >= policy.MaxRuns
		if !windowExceeded && !maxRunsExceeded {
			continue
		}
		result := run.Result
		switch {
		case windowExceeded && maxRunsExceeded:
			result.Reason = "window,maxRuns"
		case windowExceeded:
			result.Reason = "window"
		default:
			result.Reason = "maxRuns"
		}
		candidates = append(candidates, result)
	}
	return candidates
}

func pruneOne(candidate Result, db *rollup.DB) (bool, error) {
	reserved, err := journal.ReserveTerminalForPrune(candidate.RunDir)
	if err != nil {
		return false, fmt.Errorf("telemetry retention: reserve run %s: %w", candidate.RunID, err)
	}
	if !reserved {
		return false, nil
	}

	staged := stagedRunDir(candidate.RunDir)
	if err := os.MkdirAll(filepath.Dir(staged), 0o755); err != nil {
		return false, errors.Join(
			fmt.Errorf("telemetry retention: create staging directory for run %s: %w", candidate.RunID, err),
			journal.ClearPruneReservation(candidate.RunDir),
		)
	}
	if err := os.Rename(candidate.RunDir, staged); err != nil {
		clearErr := journal.ClearPruneReservation(candidate.RunDir)
		if errors.Is(err, fs.ErrNotExist) && clearErr == nil {
			return false, nil
		}
		return false, errors.Join(
			fmt.Errorf("telemetry retention: stage run %s: %w", candidate.RunID, err),
			clearErr,
		)
	}
	if err := db.DeleteRun(candidate.RunID); err != nil {
		rollbackErr := os.Rename(staged, candidate.RunDir)
		if rollbackErr == nil {
			rollbackErr = journal.ClearPruneReservation(candidate.RunDir)
		}
		return false, errors.Join(
			fmt.Errorf("telemetry retention: delete rollup rows for run %s: %w", candidate.RunID, err),
			rollbackErr,
		)
	}
	if err := os.RemoveAll(staged); err != nil {
		return false, fmt.Errorf("telemetry retention: remove staged journal for run %s: %w", candidate.RunID, err)
	}
	return true, nil
}

func stagedRunDir(runDir string) string {
	runRoot := filepath.Dir(runDir)
	return filepath.Join(stagingRoot(runRoot), filepath.Base(runDir))
}

func stagingRoot(runRoot string) string {
	return filepath.Join(filepath.Dir(runRoot), stagingDirName)
}

func finishInterruptedPrunes(runRoots []string, db *rollup.DB) error {
	for _, root := range runRoots {
		stagedRoot := stagingRoot(root)
		entries, err := os.ReadDir(stagedRoot)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("telemetry retention: read %s for interrupted prunes: %w", stagedRoot, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			dir := filepath.Join(stagedRoot, entry.Name())
			runID := entry.Name()
			// DeleteRun commits before RemoveAll begins, so a staged directory
			// whose run.yaml was already unlinked only needs idempotent cleanup.
			reader, openErr := journal.OpenRead(dir)
			if openErr == nil {
				phase, phaseErr := reader.Phase()
				if phaseErr != nil {
					return fmt.Errorf("telemetry retention: read staged run %s phase: %w", runID, phaseErr)
				}
				if phase == journal.PhaseRunning {
					return fmt.Errorf("telemetry retention: staged run %s is not terminal", runID)
				}
				identity, identityErr := reader.Identity()
				if identityErr != nil {
					return fmt.Errorf("telemetry retention: read staged run %s identity: %w", runID, identityErr)
				}
				runID = identity.RunID
			} else if !errors.Is(openErr, fs.ErrNotExist) {
				return fmt.Errorf("telemetry retention: open staged run %s: %w", runID, openErr)
			}
			if err := db.DeleteRun(runID); err != nil {
				return fmt.Errorf("telemetry retention: finish rollup prune for run %s: %w", runID, err)
			}
			if err := os.RemoveAll(dir); err != nil {
				return fmt.Errorf("telemetry retention: finish journal prune for run %s: %w", runID, err)
			}
		}
	}
	return nil
}
