package rollup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

// Rebuild derives telemetry.db from scratch by wiping any existing rollup at
// dbPath and re-ingesting every run directory under runsDir plus the instance
// journal and spans at schedulerDir. The lifetime first-success milestone is
// carried forward when the old rollup is readable because retention may already
// have removed its source run. All other rollup data remains a journal-derived
// projection (TEL-032). This is the primitive behind `goobers telemetry
// --rebuild`.
//
// Run directories are processed in sorted-name order so a rebuild is
// deterministic run-over-run; each run's own IngestRun is itself idempotent
// (delete-then-insert), so the resulting rows are identical regardless of
// processing order or whether a run was previously ingested incrementally.
func Rebuild(ctx context.Context, dbPath, runsDir, schedulerDir string) error {
	return RebuildAll(ctx, dbPath, []string{runsDir}, schedulerDir)
}

// RebuildAll derives telemetry.db from every per-gaggle run root.
//
// Takes a context because a rebuild is the longest operation in the system —
// measured at ~51 s over 29,759 runs at 1x, and proportionally longer at 3x/10x
// — so an operator who interrupts it must have it actually stop. The
// context-threading pass initially resolved this the other way, calling
// context.Background() at each statement inside the loop, which compiled and ran
// but left the whole rebuild uncancellable.
func RebuildAll(ctx context.Context, dbPath string, runsDirs []string, schedulerDir string) error {
	maintenanceLocks, err := journal.AcquireRunRootMaintenanceLocks(runsDirs)
	if err != nil {
		return err
	}
	defer func() { _ = maintenanceLocks.Release() }()

	firstSuccess := existingTimeToFirstPR(ctx, dbPath)
	runDirectories, err := rebuildRunDirs(runsDirs)
	if err != nil {
		return err
	}
	for _, dir := range runDirectories {
		if _, err := journal.OpenRead(dir); err != nil {
			return fmt.Errorf("rollup: admit %s: %w", dir, err)
		}
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
		ctx,
		timeOrZero(firstSuccess.InitCompletedAt),
		timeOrZero(firstSuccess.FirstPROpenAt),
	); err != nil {
		return err
	}
	for _, dir := range runDirectories {
		if err := db.IngestRun(ctx, dir); err != nil {
			return fmt.Errorf("rollup: ingest %s: %w", dir, err)
		}
	}
	if err := db.rebuildSchedulerLog(ctx, schedulerDir); err != nil {
		return fmt.Errorf("rollup: ingest scheduler log %s: %w", schedulerDir, err)
	}
	return nil
}

func rebuildRunDirs(runsDirs []string) ([]string, error) {
	roots := append([]string(nil), runsDirs...)
	sort.Strings(roots)
	var directories []string
	for _, runsDir := range roots {
		dirs, err := runDirs(runsDir)
		if err != nil {
			return nil, err
		}
		directories = append(directories, dirs...)
	}
	return directories, nil
}

// existingTimeToFirstPR is best-effort so an unreadable projection cannot
// prevent an explicit rebuild. Journal ingestion repopulates any milestone that
// retention has not already removed.
func existingTimeToFirstPR(ctx context.Context, dbPath string) telemetry.TimeToFirstPRMetric {
	empty := telemetry.NewTimeToFirstPRMetric(time.Time{}, time.Time{})
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return empty
	} else if err != nil {
		return empty
	}
	db, err := Open(dbPath)
	if err != nil {
		return empty
	}
	metric, queryErr := db.TimeToFirstPR(ctx)
	closeErr := db.Close()
	if queryErr != nil || closeErr != nil {
		return empty
	}
	return metric
}

func timeOrZero(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return *value
}

// runDirs lists the immediate subdirectories of runsDir that contain either a
// current schema marker or the legacy run marker, sorted by name for
// deterministic processing order. A missing runsDir is not an error.
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
		if !journal.Recorded(dir) {
			continue
		}
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs, nil
}
