package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

const telemetryCompactHelp = "Usage: goobers telemetry compact [--dry-run] [path]\n\n" +
	"Reclaim disk from a bloated instance: drop scheduler journal records\n" +
	"(scheduler/events.jsonl) and rollup scheduler rows older than\n" +
	"telemetry.retention.window, then VACUUM the rollup database and truncate its\n" +
	"write-ahead log so freed pages return to the filesystem. The daemon must be\n" +
	"stopped first — compaction rewrites files a running daemon holds open.\n" +
	"--dry-run reports what would be reclaimed without changing anything. The\n" +
	"default 90d window applies when none is configured. Exit codes: 0 = OK,\n" +
	"1 = compaction error, 2 = usage/config error.\n"

func runTelemetryCompact(args []string, stdout, stderr io.Writer) int {
	return runTelemetryCompactAt(args, stdout, stderr, time.Now())
}

func runTelemetryCompactAt(args []string, stdout, stderr io.Writer, now time.Time) int {
	fs := flag.NewFlagSet("telemetry compact", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "report what would be reclaimed without changing anything")
	fs.Usage = helpUsage(stderr, "telemetry compact")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}

	layout := instance.NewLayout(root)
	config, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	retentionConfig := instance.TelemetryRetentionConfig{}
	if config.Telemetry.Retention != nil {
		retentionConfig = *config.Telemetry.Retention
	}
	window, err := retentionConfig.WindowDuration()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	// Compaction rewrites events.jsonl and VACUUMs telemetry.db — both of which
	// a running daemon holds open for append/exclusive write. Replacing them out
	// from under it strands writes or races the VACUUM rewrite, so refuse rather
	// than corrupt.
	running, _, _, err := inspectDaemonLiveness(filepath.Join(layout.SchedulerDir(), "up.lock"), now)
	if err != nil {
		pf(stderr, "error: check daemon liveness: %v\n", err)
		return 2
	}
	if running {
		pf(stderr, "error: a daemon is running for this instance; stop it before compacting (compaction rewrites files the daemon holds open)\n")
		return 2
	}

	cutoff := now.Add(-window)
	verb := "compacted"
	if *dryRun {
		verb = "would compact"
	}

	// 1. Scheduler journal (scheduler/events.jsonl) — the source of the #1410
	//    bloat. Records older than the retention window are dropped; kept records
	//    keep their original seq so an incremental consumer just re-reads.
	journalResult, err := journal.CompactInstanceEvents(layout.SchedulerDir(), cutoff, *dryRun)
	if err != nil {
		pf(stderr, "error: compact scheduler journal: %v\n", err)
		return 1
	}
	pf(stdout, "%s scheduler journal: %d record(s) dropped (%s -> %s)\n",
		verb, journalResult.Dropped, formatByteSize(journalResult.BeforeBytes), formatByteSize(journalResult.AfterBytes))

	// 2. Rollup scheduler rows + VACUUM — deletes never shrink the file on their
	//    own, so the aged scheduler_events/scheduler_errors rows must be pruned
	//    and the freed pages reclaimed.
	dbPath := layout.TelemetryDB()
	if _, statErr := os.Stat(dbPath); os.IsNotExist(statErr) {
		return 0 // no rollup db yet — journal compaction is all there is to do
	}
	dbBefore := fileSizeOrZero(dbPath) + fileSizeOrZero(dbPath+"-wal")
	if *dryRun {
		pf(stdout, "would prune rollup scheduler rows older than %s and VACUUM the telemetry db (currently %s)\n",
			cutoff.UTC().Format(time.RFC3339), formatByteSize(dbBefore))
		return 0
	}
	db, err := rollup.Open(dbPath)
	if err != nil {
		pf(stderr, "error: open telemetry db: %v\n", err)
		return 1
	}
	prunedRows, err := db.PruneSchedulerBefore(cutoff)
	if err != nil {
		_ = db.Close()
		pf(stderr, "error: prune scheduler rollup rows: %v\n", err)
		return 1
	}
	if err := db.Compact(); err != nil {
		_ = db.Close()
		pf(stderr, "error: vacuum telemetry db: %v\n", err)
		return 1
	}
	if err := db.Close(); err != nil {
		pf(stderr, "error: close telemetry db: %v\n", err)
		return 1
	}
	dbAfter := fileSizeOrZero(dbPath) + fileSizeOrZero(dbPath+"-wal")
	pf(stdout, "compacted telemetry db: %d scheduler row(s) pruned (%s -> %s)\n",
		prunedRows, formatByteSize(dbBefore), formatByteSize(dbAfter))
	return 0
}

func fileSizeOrZero(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// formatByteSize renders a byte count in the largest unit that keeps it >= 1,
// for human-readable compaction reports.
func formatByteSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
