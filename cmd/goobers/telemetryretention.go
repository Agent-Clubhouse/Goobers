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

func compactSchedulerRetention(
	ctx context.Context,
	config instance.TelemetryRetentionConfig,
	db *rollup.DB,
	instanceLog *journal.InstanceLog,
	now time.Time,
) error {
	window, err := config.WindowDuration()
	if err != nil {
		return err
	}
	cutoff := now.Add(-window)
	budgetCutoff := now.Add(-24 * time.Hour)

	if db != nil && instanceLog != nil {
		err := db.MaintainSchedulerRetention(ctx, instanceLog.Dir(), cutoff, func() error {
			_, err := instanceLog.Compact(cutoff, budgetCutoff)
			return err
		})
		if err != nil {
			return err
		}
		return nil
	}
	if db != nil {
		if _, err := db.PruneSchedulerBefore(ctx, cutoff); err != nil {
			return fmt.Errorf("prune scheduler rollup rows: %w", err)
		}
	}
	if instanceLog != nil {
		if _, err := instanceLog.Compact(cutoff, budgetCutoff); err != nil {
			return fmt.Errorf("compact scheduler journal: %w", err)
		}
	}
	return nil
}
