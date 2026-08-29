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

// Rebuild derives telemetry.db from scratch by re-ingesting every run
// directory under runsDir plus the instance journal and spans at schedulerDir
// into a sibling staging database that atomically replaces any existing rollup
// at dbPath only once it is complete. The lifetime first-success milestone is
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
//
// The rebuild is transactional with respect to the active database: every
// journal is decoded and ingested into a sibling staging database, which is
// synced, closed and only then atomically renamed over dbPath. A malformed
// journal, a cancelled context, or any other mid-rebuild failure therefore
// leaves the last usable projection exactly as it was instead of destroying it
// and stopping partway through a fresh one.
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
	stagingPath, err := createStagingDB(dbPath)
	if err != nil {
		return err
	}
	defer removeDBFiles(stagingPath)

	if err := buildRollup(ctx, stagingPath, firstSuccess, runDirectories, schedulerDir); err != nil {
		return err
	}
	return replaceDB(dbPath, stagingPath)
}

// buildRollup populates a freshly staged database with the whole projection and
// leaves it durably on disk, closed. It never touches the active database, so
// any error here is reported with the active projection still intact.
func buildRollup(
	ctx context.Context,
	stagingPath string,
	firstSuccess telemetry.TimeToFirstPRMetric,
	runDirectories []string,
	schedulerDir string,
) error {
	db, err := Open(stagingPath)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			_ = db.Close()
		}
	}()

	if err := db.recordTimeToFirstPR(
		ctx,
		timeOrZero(firstSuccess.InitCompletedAt),
		timeOrZero(firstSuccess.FirstPROpenAt),
	); err != nil {
		return err
	}
	for _, dir := range runDirectories {
		if err := db.IngestRun(ctx, dir); err != nil {
			return fmt.Errorf("rollup: ingest %s (active rollup left unchanged): %w", dir, err)
		}
	}
	if err := db.rebuildSchedulerLog(ctx, schedulerDir); err != nil {
		return fmt.Errorf("rollup: ingest scheduler log %s (active rollup left unchanged): %w", schedulerDir, err)
	}

	// Fold the WAL back into the main file so the single staged file carries
	// the complete projection — the sidecars are discarded, not renamed.
	checkpointWAL(ctx, db.sql)
	closed = true
	if err := db.Close(); err != nil {
		return fmt.Errorf("rollup: close staged rollup %s: %w", stagingPath, err)
	}
	if err := syncFile(stagingPath); err != nil {
		return fmt.Errorf("rollup: sync staged rollup %s: %w", stagingPath, err)
	}
	return nil
}

// createStagingDB reserves a uniquely named sibling of dbPath. A sibling (same
// directory, same filesystem) is what makes the final replacement a rename
// rather than a copy, and therefore atomic.
func createStagingDB(dbPath string) (string, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("rollup: create rollup directory %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(dbPath)+".rebuild-*")
	if err != nil {
		return "", fmt.Errorf("rollup: create staging rollup next to %s: %w", dbPath, err)
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("rollup: create staging rollup %s: %w", name, err)
	}
	// os.CreateTemp creates 0600, whereas a database SQLite creates itself is
	// 0644 minus umask; the staged file has to carry the same mode it replaces.
	if err := os.Chmod(name, 0o644); err != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("rollup: prepare staging rollup %s: %w", name, err)
	}
	return name, nil
}

// replaceDB swaps the staged database in for the active one. The stale sidecars
// are removed first: a leftover -wal from the old database would otherwise be
// replayed against the renamed file, which is the one way this swap could
// publish a corrupt projection.
func replaceDB(dbPath, stagingPath string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := os.Remove(dbPath + suffix); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("rollup: remove existing %s%s: %w", dbPath, suffix, err)
		}
	}
	if err := os.Rename(stagingPath, dbPath); err != nil {
		return fmt.Errorf("rollup: replace %s with staged rollup %s: %w", dbPath, stagingPath, err)
	}
	return nil
}

// removeDBFiles discards a staged database and any sidecars it left behind. A
// successful rebuild has already renamed the main file away, so the removal of
// that name is expected to be a no-op.
func removeDBFiles(path string) {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		_ = os.Remove(path + suffix)
	}
}

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
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
