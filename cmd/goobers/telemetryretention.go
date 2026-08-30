package main

import (
	"context"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/retention"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

const telemetryRetentionSweepInterval = 6 * time.Hour

func pruneTelemetryRetention(
	layout instance.Layout,
	config instance.TelemetryRetentionConfig,
	db *rollup.DB,
	now time.Time,
	dryRun bool,
) ([]retention.Result, error) {
	window, err := config.WindowDuration()
	if err != nil {
		return nil, err
	}
	policy := retention.Policy{Window: window, MaxRuns: config.MaxRunLimit()}

	ownedDB := false
	if !dryRun && db == nil {
		db, err = rollup.Open(layout.TelemetryDB())
		if err != nil {
			return nil, err
		}
		ownedDB = true
	}
	if ownedDB {
		defer func() { _ = db.Close() }()
	}
	return retention.Prune(layout, db, policy, retention.Options{Now: now, DryRun: dryRun})
}

func pruneConfiguredTelemetryRetention(
	layout instance.Layout,
	config instance.TelemetryRetentionConfig,
	db *rollup.DB,
	now time.Time,
) ([]retention.Result, error) {
	if !config.Enabled {
		return nil, nil
	}
	return pruneTelemetryRetention(layout, config, db, now, false)
}

// compactSchedulerRetention bounds the scheduler journal and rollup rows. A
// stale-generation cleanup failure is reported through cleanupErrors (a nil
// reporter simply drops it) rather than returned: the compaction itself
// recorded new data and succeeded, so failing the whole sweep over disk a
// later compaction will reclaim anyway would be wrong — but on the daemon's
// unattended path this is the only chance to make the failure observable.
func compactSchedulerRetention(
	ctx context.Context,
	config instance.TelemetryRetentionConfig,
	db *rollup.DB,
	instanceLog *journal.InstanceLog,
	cleanupErrors *sweepErrorReporter,
	now time.Time,
) error {
	window, err := config.WindowDuration()
	if err != nil {
		return err
	}
	cutoff := now.Add(-window)
	budgetCutoff := now.Add(-24 * time.Hour)

	reportCleanup := func(result journal.InstanceEventsCompaction) {
		if cleanupErrors == nil {
			return
		}
		cleanupErrors.report(result.StaleGenerationCleanupErr)
	}

	if db != nil && instanceLog != nil {
		var compaction journal.InstanceEventsCompaction
		compacted := false
		err := db.MaintainSchedulerRetention(ctx, instanceLog.Dir(), cutoff, func() error {
			result, err := instanceLog.Compact(cutoff, budgetCutoff)
			if err != nil {
				return err
			}
			compaction = result
			compacted = true
			return nil
		})
		if err != nil {
			return err
		}
		// Only a compaction that actually ran carries a verdict about stale
		// generations. Reporting the zero value when the closure never fired
		// would clear a real consecutive-failure streak with no evidence.
		if compacted {
			reportCleanup(compaction)
		}
		return nil
	}
	if db != nil {
		if _, err := db.PruneSchedulerBefore(ctx, cutoff); err != nil {
			return fmt.Errorf("prune scheduler rollup rows: %w", err)
		}
	}
	if instanceLog != nil {
		result, err := instanceLog.Compact(cutoff, budgetCutoff)
		if err != nil {
			return fmt.Errorf("compact scheduler journal: %w", err)
		}
		reportCleanup(result)
	}
	return nil
}
