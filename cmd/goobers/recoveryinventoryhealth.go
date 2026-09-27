package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/recovery"
)

// recoveryInventorySampleInterval is how often the daemon re-measures shared
// recovery-inventory occupancy. Var, not const, so tests drive the loop
// without waiting out a real interval.
//
// The cadence is a minute rather than a tick: the scan takes the instance-wide
// inventory lock that publication, retirement and the reap also hold, and the
// condition it is watching for builds over hours, not milliseconds.
var recoveryInventorySampleInterval = time.Minute

// recoveryInventoryWarningStateFile is where the high-water warning's dedupe
// state lives, next to the retention grace states in the scheduler directory.
//
// Durable on purpose. An in-memory flag re-warns on every daemon start, and a
// daemon on a wedged instance restarts often — which is precisely when a
// repeated "inventory exhausted" record is least informative and most likely
// to be scrolled past. Persisting the last reported state means a restart with
// the condition unchanged says nothing new.
const recoveryInventoryWarningStateFile = "recovery-inventory-warning.json"

const recoveryInventoryWarningSchema = "goobers.recovery-inventory-warning/v1"

// recoveryInventoryWarningState is the last occupancy state an operator was
// told about.
type recoveryInventoryWarningState struct {
	Schema string `json:"schema"`
	// State is the last REPORTED state, not the last observed one: an
	// unavailable reading leaves it untouched so a transient read failure
	// cannot re-arm the warning and produce a duplicate on the next sample.
	State      string    `json:"state"`
	ReportedAt time.Time `json:"reportedAt,omitempty"`
}

// recoveryInventorySeverity ranks occupancy states so a transition can be
// tested as an escalation rather than as any change at all. Warning is
// emitted when the rank RISES, and re-armed only by a fall.
func recoveryInventorySeverity(state string) int {
	switch state {
	case readservice.RecoveryInventoryWarning:
		return 1
	case readservice.RecoveryInventoryExhausted:
		return 2
	default:
		return 0
	}
}

// recoveryInventoryGate holds the most recent sample for the read service.
// The zero value is not usable; newRecoveryInventoryGate builds one.
type recoveryInventoryGate struct {
	layout instance.Layout
	// cfg is the daemon's carried configuration. It is the FALLBACK for
	// policy resolution, never the source: resolveRecoveryPolicy re-reads
	// instance.yaml at every sample, so raising or lowering
	// retention.recovery.maxSnapshots is reflected in what this reports
	// without restarting the daemon (#4911, 2026-09-17 correction).
	cfg     *instance.Config
	now     func() time.Time
	current atomic.Pointer[readservice.RecoveryInventoryStatus]
}

func newRecoveryInventoryGate(layout instance.Layout, cfg *instance.Config, now func() time.Time) *recoveryInventoryGate {
	if now == nil {
		now = time.Now
	}
	return &recoveryInventoryGate{layout: layout, cfg: cfg, now: now}
}

// Stats returns the last sample, or nil before the first one completes.
func (g *recoveryInventoryGate) Stats() *readservice.RecoveryInventoryStatus {
	return g.current.Load()
}

// Sample re-measures occupancy and stores the result.
func (g *recoveryInventoryGate) Sample(ctx context.Context) *readservice.RecoveryInventoryStatus {
	status := g.observe(ctx)
	g.current.Store(status)
	return status
}

func (g *recoveryInventoryGate) observe(ctx context.Context) *readservice.RecoveryInventoryStatus {
	policy, origin := resolveRecoveryPolicy(g.layout, g.cfg)
	limit := policy.MaxSnapshotsEffective()
	root := filepath.Join(g.layout.Root, "recovery")
	status := &readservice.RecoveryInventoryStatus{
		Limit:            limit,
		HighWaterPercent: readservice.RecoveryInventoryHighWaterPercent,
		InventoryRoot:    root,
		PolicySource:     origin.Source,
		ObservedAt:       g.now().UTC(),
	}
	// Read at the structural ceiling, not at the operator's cap, and compare
	// against the cap here.
	//
	// A reader that passes the policy limit down is refused outright once the
	// directory holds MORE entries than the cap — the "130 of 128" shape, in
	// which the READ refuses rather than the reservation (#5300, #5354). That
	// is correct for a writer and useless for an observer: capacity reporting
	// would go blind at exactly the occupancy it exists to report. Tolerant
	// for the same reason: one crashed publish leaving a lock-only directory
	// must not take the reading down with it (#5177).
	entries, unreadable, err := recovery.ReadInventoryTolerant(ctx, root, recovery.MaxInventoryEntries)
	if err != nil {
		status.State = readservice.RecoveryInventoryUnavailable
		status.Error = recoveryInventoryReadError(err, limit).Error()
		return status
	}
	// Incomplete reservations occupy slots until they are reconciled, so they
	// are counted in Used as well as reported separately: Used has to be the
	// same number the next refused cleanup will name.
	status.Used = len(entries) + len(unreadable)
	status.Unreadable = len(unreadable)
	// A failed overflow reading is reported as unavailable rather than as
	// zero: "no overflow" and "could not tell" must not look alike in the one
	// place the condition is visible at all.
	overflow, overflowErr := recoveryOverflowCount(ctx, g.layout)
	if overflowErr != nil {
		status.State = readservice.RecoveryInventoryUnavailable
		status.Error = overflowErr.Error()
		return status
	}
	status.Overflow = overflow
	status.State = readservice.ClassifyRecoveryInventory(status.Used, limit, overflow)
	if earliest, ok := earliestRetainUntil(entries); ok {
		status.EarliestRetainUntil = &earliest
	}
	return status
}

// earliestRetainUntil reports when the next slot can be reclaimed by policy.
// An entry whose record cannot be read is skipped rather than failing the
// whole reading: the deadline is supporting detail, the occupancy is not.
func earliestRetainUntil(entries []recovery.InventoryEntry) (time.Time, bool) {
	var earliest time.Time
	for _, entry := range entries {
		// The reservation's own record carries the deadline it was published
		// with; renewals live in the sidecar, so only ReadRetainedRecord
		// reports the effective deadline.
		record, err := recovery.ReadRetainedRecord(entry.RecordPath)
		if err != nil {
			continue
		}
		if earliest.IsZero() || record.RetainUntil.Before(earliest) {
			earliest = record.RetainUntil
		}
	}
	if earliest.IsZero() {
		return time.Time{}, false
	}
	return earliest.UTC(), true
}

// readRecoveryInventoryWarningState returns the last reported state, or the
// zero value when nothing has been reported on this instance yet.
func readRecoveryInventoryWarningState(layout instance.Layout) recoveryInventoryWarningState {
	data, err := os.ReadFile(filepath.Join(layout.SchedulerDir(), recoveryInventoryWarningStateFile))
	if err != nil {
		return recoveryInventoryWarningState{}
	}
	var state recoveryInventoryWarningState
	if err := json.Unmarshal(data, &state); err != nil {
		// Unreadable dedupe state re-arms the warning. Repeating a warning is
		// a smaller failure than silently withholding one.
		return recoveryInventoryWarningState{}
	}
	return state
}

func writeRecoveryInventoryWarningState(layout instance.Layout, state recoveryInventoryWarningState) error {
	state.Schema = recoveryInventoryWarningSchema
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		return err
	}
	return journal.WriteFileAtomic(filepath.Join(layout.SchedulerDir(), recoveryInventoryWarningStateFile), data, 0o644)
}

// recoveryInventoryWarningMessage is what an operator reads in the instance
// log. It names the coupling, because the symptom they will otherwise see is
// an unrelated run failing at `create worktree` (#5354).
func recoveryInventoryWarningMessage(status *readservice.RecoveryInventoryStatus) string {
	message := fmt.Sprintf(
		"recovery inventory %s: %d of %d slots used in %s; durable handoffs, worktree cleanup and unrelated runs fail when it is full",
		status.State, status.Used, status.Limit, status.InventoryRoot,
	)
	if status.Unreadable > 0 {
		message += fmt.Sprintf(" (%d incomplete reservation(s) occupying slots)", status.Unreadable)
	}
	if status.Overflow > 0 {
		// Name what overflow actually costs. It is not the pre-#5370 wedge —
		// cleanup succeeded — so an operator must not read this as stopped
		// execution, and must not read it as nothing either.
		message += fmt.Sprintf("; %d snapshot(s) held as mirror refs without a bundle until capacity frees", status.Overflow)
	}
	if status.EarliestRetainUntil != nil {
		message += fmt.Sprintf("; earliest retention deadline %s", status.EarliestRetainUntil.Format(time.RFC3339))
	}
	return message
}

func recoveryInventoryWarningCode(state string) string {
	if state == readservice.RecoveryInventoryExhausted {
		return "recovery_inventory_exhausted"
	}
	return "recovery_inventory_high_water"
}

// reportRecoveryInventoryHealth journals a high-water crossing exactly once.
//
// Once on the way into warning, once on the way into exhausted, and nothing at
// all while the state is unchanged — a per-sample record would be a minute-by
// -minute repetition of a condition that persists for hours. Dropping back
// below a threshold re-arms it, so a second episode is reported as a second
// episode.
func reportRecoveryInventoryHealth(layout instance.Layout, log *journal.InstanceLog, status *readservice.RecoveryInventoryStatus, now time.Time) {
	if status == nil || status.State == readservice.RecoveryInventoryUnavailable {
		return
	}
	previous := readRecoveryInventoryWarningState(layout)
	severity := recoveryInventorySeverity(status.State)
	if severity <= recoveryInventorySeverity(previous.State) {
		if severity != recoveryInventorySeverity(previous.State) {
			// A fall re-arms: record the lower state so the next rise warns.
			_ = writeRecoveryInventoryWarningState(layout, recoveryInventoryWarningState{State: status.State, ReportedAt: previous.ReportedAt})
		}
		return
	}
	if log != nil {
		log.AppendBestEffort(journal.Event{
			Type: journal.EventError,
			Error: &journal.ErrorDetail{
				Code:    recoveryInventoryWarningCode(status.State),
				Message: recoveryInventoryWarningMessage(status),
			},
		})
	}
	_ = writeRecoveryInventoryWarningState(layout, recoveryInventoryWarningState{State: status.State, ReportedAt: now.UTC()})
}

// startDaemonRecoveryInventoryHealth builds the gate and takes the startup
// sample, mirroring startDaemonStorageHealth: an instance that is already
// exhausted at boot says so before it fails its first stage.
func startDaemonRecoveryInventoryHealth(ctx context.Context, layout instance.Layout, setup *schedulerSetup) *recoveryInventoryGate {
	gate := newRecoveryInventoryGate(layout, setup.Config, nil)
	reportRecoveryInventoryHealth(layout, setup.InstanceLog, gate.Sample(ctx), time.Now())
	return gate
}

// startRecoveryInventoryTicker re-samples occupancy for the life of the
// daemon. Pulled out of the daemon's run function for the same complexity
// reason as startStorageHealthTicker.
func startRecoveryInventoryTicker(ctx context.Context, layout instance.Layout, setup *schedulerSetup, gate *recoveryInventoryGate, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	if interval <= 0 {
		close(done)
		return done
	}
	ticker := time.NewTicker(interval)
	go func() {
		defer close(done)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if ctx.Err() != nil {
					return
				}
				reportRecoveryInventoryHealth(layout, setup.InstanceLog, gate.Sample(ctx), time.Now())
			}
		}
	}()
	return done
}
