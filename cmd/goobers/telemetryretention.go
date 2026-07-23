package main

import (
	"time"

	"github.com/goobers/goobers/internal/instance"
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
