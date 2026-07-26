package rollup

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/goobers/goobers/internal/telemetry"
)

// Rebuild derives telemetry.db from scratch by wiping any existing rollup at
// dbPath and re-ingesting every run directory under runsDir plus the instance
// journal and spans at schedulerDir. The lifetime onboarding milestone is
// carried forward because retention may already have removed its source run.
// All other rollup data remains a journal-derived projection (TEL-032). This is
// the primitive behind `goobers telemetry --rebuild`.
//
// Run directories are processed in sorted-name order so a rebuild is
// deterministic run-over-run; each run's own IngestRun is itself idempotent
// (delete-then-insert), so the resulting rows are identical regardless of
// processing order or whether a run was previously ingested incrementally.
func Rebuild(dbPath, runsDir, schedulerDir string) error {
	return RebuildAll(dbPath, []string{runsDir}, schedulerDir)
}

// RebuildAll derives telemetry.db from every per-gaggle run root.
func RebuildAll(dbPath string, runsDirs []string, schedulerDir string) error {
	onboarding, err := existingTimeToFirstPR(dbPath)
	if err != nil {
		return err
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := os.Remove(dbPath + suffix); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("rollup: remove existing %s%s: %w", dbPath, suffix, err)
		}
	}

	db, err := Open(dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if err := db.recordTimeToFirstPR(
		timeOrZero(onboarding.FirstRunAt),
		timeOrZero(onboarding.FirstPROpenAt),
	); err != nil {
		return err
	}
	roots := append([]string(nil), runsDirs...)
	sort.Strings(roots)
	for _, runsDir := range roots {
		dirs, err := runDirs(runsDir)
		if err != nil {
			return err
		}
		for _, dir := range dirs {
			if err := db.IngestRun(dir); err != nil {
				return fmt.Errorf("rollup: ingest %s: %w", dir, err)
			}
		}
	}
	if err := db.IngestSchedulerLog(schedulerDir); err != nil {
		return fmt.Errorf("rollup: ingest scheduler log %s: %w", schedulerDir, err)
	}
	return nil
}

func existingTimeToFirstPR(dbPath string) (telemetry.TimeToFirstPRMetric, error) {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return telemetry.NewTimeToFirstPRMetric(time.Time{}, time.Time{}), nil
	} else if err != nil {
		return telemetry.TimeToFirstPRMetric{}, fmt.Errorf("rollup: inspect existing database: %w", err)
	}
	db, err := Open(dbPath)
	if err != nil {
		return telemetry.TimeToFirstPRMetric{}, fmt.Errorf("rollup: preserve onboarding milestone: %w", err)
	}
	metric, queryErr := db.TimeToFirstPR()
	closeErr := db.Close()
	if queryErr != nil {
		return telemetry.TimeToFirstPRMetric{}, queryErr
	}
	if closeErr != nil {
		return telemetry.TimeToFirstPRMetric{}, fmt.Errorf("rollup: close existing database: %w", closeErr)
	}
	return metric, nil
}

func timeOrZero(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return *value
}

// runDirs lists the immediate subdirectories of runsDir that look like a run
// (contain run.yaml), sorted by name for deterministic processing order. A
// missing runsDir is not an error — it means no runs exist yet.
func runDirs(runsDir string) ([]string, error) {
	entries, err := os.ReadDir(runsDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("rollup: read %s: %w", runsDir, err)
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(runsDir, e.Name())
		if _, err := os.Stat(filepath.Join(dir, fileRunYAML)); err != nil {
			continue
		}
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs, nil
}
