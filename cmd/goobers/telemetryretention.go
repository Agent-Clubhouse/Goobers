package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/retention"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

const telemetryRetentionSweepInterval = 6 * time.Hour

// telemetryRetentionGraceWindow is #3056/#4253's "safe first-enable"
// duration: the first time an instance's existing run telemetry is found to
// already exceed the (now opt-out by default) retention policy, pruning
// stays a dry run — reporting exactly what would be deleted, deleting
// nothing — for this long before real enforcement begins.
const telemetryRetentionGraceWindow = 7 * 24 * time.Hour

// telemetryRetentionStateFile names the durable marker
// pruneConfiguredTelemetryRetention reads and rewrites on every pass, kept
// under SchedulerDir alongside the daemon's other operational state (e.g.
// pending-triggers). It is derived, not authoritative — deleting it merely
// restarts grace-window detection from the next pass, the same as a fresh
// instance.
const telemetryRetentionStateFile = "telemetry-retention-state.json"

const telemetryRetentionStateSchema = "goobers.dev/telemetry-retention-state/v1"

// telemetryRetentionState is #4253's status-surface record: `goobers status`
// (reportTelemetryRetentionPolicy) reads it to show the policy in force, the
// last pass, and its candidate count — the ruling's "status + portal
// permanently surface" requirement — and pruneConfiguredTelemetryRetention
// itself reads it to decide whether a grace window is already running (and,
// if so, whether it has elapsed) rather than re-deciding from scratch on
// every restart.
type telemetryRetentionState struct {
	Schema string `json:"schema"`
	// DetectedAt/EnforceAt are zero until the first pass that actually finds
	// data exceeding policy starts the grace window; EnforceAt is when real
	// deletion begins (DetectedAt + telemetryRetentionGraceWindow).
	DetectedAt     time.Time `json:"detectedAt,omitempty"`
	EnforceAt      time.Time `json:"enforceAt,omitempty"`
	LastPassAt     time.Time `json:"lastPassAt"`
	LastPassDryRun bool      `json:"lastPassDryRun"`
	CandidateCount int       `json:"candidateCount"`
	PrunedCount    int       `json:"prunedCount"`
}

func telemetryRetentionStatePath(layout instance.Layout) string {
	return filepath.Join(layout.SchedulerDir(), telemetryRetentionStateFile)
}

// readTelemetryRetentionState returns ok=false (zero state, nil error) when
// no pass has ever recorded one yet — a fresh instance, or one that predates
// this file.
func readTelemetryRetentionState(layout instance.Layout) (state telemetryRetentionState, ok bool, err error) {
	data, err := os.ReadFile(telemetryRetentionStatePath(layout))
	if os.IsNotExist(err) {
		return telemetryRetentionState{}, false, nil
	}
	if err != nil {
		return telemetryRetentionState{}, false, fmt.Errorf("telemetry retention: read state: %w", err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return telemetryRetentionState{}, false, fmt.Errorf("telemetry retention: decode state: %w", err)
	}
	return state, true, nil
}

func writeTelemetryRetentionState(layout instance.Layout, state telemetryRetentionState) error {
	state.Schema = telemetryRetentionStateSchema
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("telemetry retention: encode state: %w", err)
	}
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		return fmt.Errorf("telemetry retention: create scheduler dir: %w", err)
	}
	if err := journal.WriteFileAtomic(telemetryRetentionStatePath(layout), data, 0o644); err != nil {
		return fmt.Errorf("telemetry retention: write state: %w", err)
	}
	return nil
}

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
	guard, closeGuard, err := openTriggerPruneGuard(layout, dryRun, now)
	if err != nil {
		return nil, err
	}
	defer closeGuard()
	return retention.Prune(layout, db, policy, retention.Options{Now: now, DryRun: dryRun, BeforeDelete: guard})
}

// pruneConfiguredTelemetryRetention runs one retention pass, honoring
// #4253's opt-out-by-default policy and the #3056 ruling's safe first-enable
// semantics: the pass is a dry run (reports candidates, deletes nothing)
// whenever a grace window is running or being started, and switches to real
// enforcement once that window elapses (or immediately, if the operator set
// telemetry.retention.firstEnable: immediate). The returned dryRun value
// tells the caller which happened, since an identical []retention.Result
// means something very different in each case.
func pruneConfiguredTelemetryRetention(
	layout instance.Layout,
	config instance.TelemetryRetentionConfig,
	db *rollup.DB,
	now time.Time,
) (results []retention.Result, dryRun bool, err error) {
	if !config.EnabledEffective() {
		return nil, false, nil
	}

	state, hasState, err := readTelemetryRetentionState(layout)
	if err != nil {
		return nil, false, err
	}
	immediate := config.ImmediateFirstEnable()
	withinGrace := !state.EnforceAt.IsZero() && now.Before(state.EnforceAt)
	// A fresh instance (no state yet) also runs dry — this is the probe pass
	// that decides whether a grace window needs to start at all. An instance
	// with nothing yet to prune stays harmlessly dry-run forever (there is
	// nothing a real pass would do differently), only actually starting the
	// window the first time it finds real candidates.
	dryRun = !immediate && (withinGrace || !hasState)

	results, err = pruneTelemetryRetention(layout, config, db, now, dryRun)
	if err != nil {
		return nil, dryRun, err
	}

	if dryRun && !immediate && state.EnforceAt.IsZero() && len(results) > 0 {
		state.DetectedAt = now
		state.EnforceAt = now.Add(telemetryRetentionGraceWindow)
	}
	state.LastPassAt = now
	state.LastPassDryRun = dryRun
	state.CandidateCount = len(results)
	if !dryRun {
		state.PrunedCount = len(results)
	}
	if err := writeTelemetryRetentionState(layout, state); err != nil {
		return results, dryRun, err
	}
	return results, dryRun, nil
}

// reportTelemetryPruned prints one startup-log line per pruneConfiguredTelemetryRetention
// result, wording it correctly for whichever pass produced it: a dry run
// (#4253's grace window) reports candidates without claiming anything was
// deleted, a real pass reports what actually was. Factored out of
// runUpContextWithForce (rather than inlined at its one call site) so this
// dryRun/real branch doesn't grow that already-large function's cyclomatic
// complexity — the same reason startPeriodicSweep exists (#4323).
func reportTelemetryPruned(stdout io.Writer, results []retention.Result, dryRun bool) {
	for _, result := range results {
		if dryRun {
			// Reporting only, not yet deleted — see telemetry-retention-state.json
			// / `goobers status`.
			pf(stdout, "telemetry retention candidate (grace period, not deleted) run=%q reason=%s\n", result.RunID, result.Reason)
			continue
		}
		pf(stdout, "telemetry pruned run=%q reason=%s\n", result.RunID, result.Reason)
	}
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
