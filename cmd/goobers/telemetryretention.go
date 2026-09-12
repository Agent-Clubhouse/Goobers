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

// telemetryRetentionLargeFirstEnforceFraction is #4824's second safety net,
// on top of the shared timed grace window: even after the grace window has
// elapsed, a FIRST real enforcement pass that would prune more than this
// fraction of an instance's current run history stays dry-run instead of
// proceeding automatically. The timed window alone is fine for a handful of
// stale runs beyond policy — that is exactly what it exists to let an
// operator notice and, if they disagree, react to. It is not fine for
// wiping the bulk of an instance's history on a stock config nobody typed
// (the reported case: 96.4% of history queued for deletion because
// DefaultTelemetryRetentionMaxRuns binds in ~33 hours at production run
// rates, long before the documented 90-day window ever would). Once a real
// enforcement pass has actually run once without hitting this gate, later
// passes never need it again — a policy that has already deleted anything
// for real has already been exercised, whether or not this instance
// happened to start under it.
const telemetryRetentionLargeFirstEnforceFraction = 0.5

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
type telemetryRetentionState = retentionGraceState

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

// telemetryRetentionPassIsDryRun is the telemetry policy's named adapter to
// the shared first-enable decision. Keeping the adapter makes it possible to
// prove both deletion paths stay equivalent without maintaining two copies of
// the decision itself.
func telemetryRetentionPassIsDryRun(state telemetryRetentionState, immediate bool, now time.Time) bool {
	return retentionPassIsDryRun(state, immediate, now)
}

func pruneTelemetryRetention(
	layout instance.Layout,
	config instance.TelemetryRetentionConfig,
	db *rollup.DB,
	now time.Time,
	dryRun bool,
) ([]retention.Result, retention.Summary, error) {
	window, err := config.WindowDuration()
	if err != nil {
		return nil, retention.Summary{}, err
	}
	policy := retention.Policy{Window: window, MaxRuns: config.MaxRunLimit()}

	ownedDB := false
	if !dryRun && db == nil {
		db, err = rollup.Open(layout.TelemetryDB())
		if err != nil {
			return nil, retention.Summary{}, err
		}
		ownedDB = true
	}
	if ownedDB {
		defer func() { _ = db.Close() }()
	}
	triggerGuard, closeTriggerGuard, err := openTriggerPruneGuard(layout, dryRun, now)
	if err != nil {
		return nil, retention.Summary{}, err
	}
	defer closeTriggerGuard()
	recoveryGuard, closeRecoveryGuard, err := openRecoveryCustodyPruneGuard(layout, dryRun)
	if err != nil {
		return nil, retention.Summary{}, err
	}
	defer closeRecoveryGuard()
	guard := combineBeforeDeleteGuards(triggerGuard, recoveryGuard)
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

	state, _, err := readTelemetryRetentionState(layout)
	if err != nil {
		return nil, false, err
	}
	state, _ = normalizeRetentionGraceState(state, now)
	immediate := config.ImmediateFirstEnable()
	// A grace window that has never started (EnforceAt still zero) also runs
	// dry — this is the probe pass that decides whether a window needs to
	// start at all. This must key off EnforceAt, not "does a state file
	// exist": the state file is written on every pass, including a dry pass
	// that found zero candidates, so gating on file existence alone (#4253's
	// original bug) let a single harmless empty pass permanently satisfy the
	// "first pass" check — the very next pass to find real candidates, no
	// matter how much later, then enforced immediately with zero grace
	// period. An instance with nothing yet to prune stays harmlessly dry-run
	// forever (there is nothing a real pass would do differently), only
	// actually starting the window the first time it finds real candidates.
	dryRun = telemetryRetentionPassIsDryRun(state, immediate, now)

	// #4824's second safety net: even once the timed grace window has fully
	// elapsed, a pass that has never actually enforced for real on this
	// instance runs a cheap dry precheck first. If enforcing now would prune
	// more than telemetryRetentionLargeFirstEnforceFraction of current run
	// history, stay dry-run and use the precheck's own results — an operator
	// who has not reviewed the config gets one more chance to notice before
	// most of their history disappears, on top of (not instead of) the timed
	// window above.
	largeFirstEnforceBlocked := false
	var summary retention.Summary
	if !dryRun && !immediate && !state.EnforceAcknowledged {
		precheckResults, precheckSummary, precheckErr := pruneTelemetryRetention(layout, config, db, now, true)
		if precheckErr != nil {
			return nil, dryRun, precheckErr
		}
		if precheckSummary.TotalRuns > 0 &&
			float64(len(precheckResults)) > float64(precheckSummary.TotalRuns)*telemetryRetentionLargeFirstEnforceFraction {
			dryRun = true
			largeFirstEnforceBlocked = true
			results, summary = precheckResults, precheckSummary
		}
	}
	if results == nil && !largeFirstEnforceBlocked {
		results, summary, err = pruneTelemetryRetention(layout, config, db, now, dryRun)
		if err != nil {
			return nil, dryRun, err
		}
	}

	if dryRun && !immediate && state.EnforceAt.IsZero() && len(results) > 0 {
		state.DetectedAt = now
		state.EnforceAt = now.Add(retentionGraceWindow)
	}
	state.LastPassAt = now
	state.LastPassDryRun = dryRun
	state.CandidateCount = len(results)
	state.TotalRuns = summary.TotalRuns
	state.OldestRetainedAt = summary.OldestRetainedStartedAt
	state.LargeFirstEnforceBlocked = largeFirstEnforceBlocked
	if !dryRun {
		state.PrunedCount = len(results)
		state.EnforceAcknowledged = true
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
func reportTelemetryPruned(stdout io.Writer, results []retention.Result, dryRun, enabled bool) {
	mode := "enforcing"
	if !enabled {
		mode = "disabled"
	} else if dryRun {
		mode = "dry-run"
	}
	pf(stdout, "telemetry retention: candidates=%d mode=%s\n", len(results), mode)
}

// recordTelemetryRetentionPass publishes the bounded operational result that
// status and portal replay. Candidate identities remain in the derived state
// file and startup no longer logs them one by one.
func recordTelemetryRetentionPass(
	log *journal.InstanceLog,
	layout instance.Layout,
	config instance.TelemetryRetentionConfig,
	results []retention.Result,
	dryRun bool,
) error {
	if log == nil || !config.EnabledEffective() {
		return nil
	}
	state, ok, err := readTelemetryRetentionState(layout)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("telemetry retention: successful pass did not persist state")
	}
	mode := "enforcing"
	if dryRun {
		mode = "dry-run"
	}
	runner := map[string]any{
		"mode":           mode,
		"candidateCount": len(results),
	}
	if !state.EnforceAt.IsZero() {
		runner["enforceAt"] = state.EnforceAt.UTC().Format(time.RFC3339Nano)
	}
	return log.Append(journal.Event{Type: journal.EventTelemetryRetentionPass, Runner: runner})
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
