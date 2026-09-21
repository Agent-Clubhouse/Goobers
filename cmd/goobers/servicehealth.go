package main

import (
	"context"
	"errors"
	"os"
	"os/user"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

// serviceHealthInterval is #5244's six-hour cadence. Var, not const, so tests
// drive the loop without waiting out a real interval.
var serviceHealthInterval = 6 * time.Hour

// serviceHealthSchemaVersion versions the record's payload shape. Consumers
// read it before interpreting any other field, so a later field addition is a
// version bump rather than a silent change of meaning for an older reader.
//
// 2 adds recoveryInventory (#4911 AC5).
const serviceHealthSchemaVersion = 2

// serviceHealthUnknown is the explicit marker for a field whose real value
// could not be observed.
//
// #5244 requires that missing identity stay explicit. An empty string would be
// indistinguishable from "observed, and empty", and a plausible-looking
// substitute (the interactive user, the instance root's basename) would be
// worse still: it would answer the question wrongly rather than declining to
// answer it.
const serviceHealthUnknown = "unknown"

// serviceHealthObservation is everything one record reports.
type serviceHealthObservation struct {
	// InstanceID is the DURABLE instance identity, not a path or a display
	// name. Empty when the root has none, which IdentityProblem then explains.
	InstanceID      string
	DisplayName     string
	IdentityProblem string
	MachineName     string
	// AccountName is the account the SERVICE is actually executing as — the
	// daemon process's own user. #5244 is explicit that this means the
	// executing service or worker, never the interactive observer, so it is
	// read from this process rather than from any ambient session.
	AccountName string
	StartedAt   time.Time
	ObservedAt  time.Time
	// UncleanRestarts counts observed dirty-restart records inside the window.
	// Zero is a real answer — a covered window with none — which is why
	// WindowCoverage is reported alongside it rather than leaving a reader to
	// guess whether zero meant "none happened" or "nothing was readable".
	UncleanRestarts int
	WindowStart     time.Time
	WindowCoverage  string
	// RecoveryInventory is shared recovery-snapshot occupancy at the moment
	// of observation (#4911 AC5). Nil when this reader has no sample — an
	// offline or pre-first-sample record declines to answer rather than
	// reporting an empty inventory it never measured.
	RecoveryInventory *readservice.RecoveryInventoryStatus
}

// Window coverage values. An incomplete history stays explicit rather than
// being presented as a clean record.
const (
	// serviceHealthWindowComplete means the lifecycle history was read in full.
	serviceHealthWindowComplete = "complete"
	// serviceHealthWindowUnknown means it could not be read, so the restart
	// count is not a measurement and must not be treated as one.
	serviceHealthWindowUnknown = "unknown"
)

// observeServiceHealth gathers one observation. Every lookup degrades to an
// explicit unknown rather than failing: a diagnostic record that refuses to be
// written when one field is unavailable reports nothing at all, which is the
// opposite of what it exists for.
func observeServiceHealth(root string, identity *daemonIdentity, log *journal.InstanceLog, inventory recoveryInventorySampler, now time.Time) serviceHealthObservation {
	obs := serviceHealthObservation{
		ObservedAt:     now.UTC(),
		MachineName:    serviceHealthUnknown,
		AccountName:    serviceHealthUnknown,
		WindowCoverage: serviceHealthWindowUnknown,
	}
	if id, err := instance.ReadRootIdentity(root); err == nil {
		obs.InstanceID = id
	} else if errors.Is(err, os.ErrNotExist) {
		obs.IdentityProblem = "root has no durable identity"
	} else {
		obs.IdentityProblem = "root identity is invalid or unreadable"
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		obs.MachineName = host
	}
	if account, err := user.Current(); err == nil && account.Username != "" {
		obs.AccountName = account.Username
	}
	if identity != nil {
		obs.StartedAt = identity.StartedAt.UTC()
	}
	if inventory != nil {
		obs.RecoveryInventory = inventory()
	}
	if log != nil {
		if events, err := journal.ReadInstanceLog(log.Dir()); err == nil {
			obs.WindowCoverage = serviceHealthWindowComplete
			obs.UncleanRestarts, obs.WindowStart = summarizeLifecycleWindow(events)
		}
	}
	return obs
}

// summarizeLifecycleWindow counts observed dirty restarts and reports when the
// readable history begins. The window start is the earliest event actually
// read, not the daemon's start time: those differ after log rotation, and
// conflating them would claim coverage the record does not have.
func summarizeLifecycleWindow(events []journal.Event) (restarts int, windowStart time.Time) {
	for _, ev := range events {
		if ev.Type == journal.EventDaemonDirtyRestart {
			restarts++
		}
		if !ev.Time.IsZero() && (windowStart.IsZero() || ev.Time.Before(windowStart)) {
			windowStart = ev.Time.UTC()
		}
	}
	return restarts, windowStart
}

// serviceHealthPayload renders the observation as the event's Runner payload.
//
// Field names are chosen to avoid over-claiming, which #5244 calls out twice:
// observedUncleanRestarts is not a confirmed crash count (a dirty restart means
// the previous lock was not cleanly released, which has other causes), and
// processUptimeSeconds is this process's lifetime, not cumulative healthy
// availability across restarts.
func serviceHealthPayload(obs serviceHealthObservation) map[string]any {
	payload := map[string]any{
		"schemaVersion":  serviceHealthSchemaVersion,
		"observedAt":     obs.ObservedAt.Format(time.RFC3339Nano),
		"machineName":    obs.MachineName,
		"accountName":    obs.AccountName,
		"windowCoverage": obs.WindowCoverage,
	}
	if obs.InstanceID != "" {
		payload["instanceId"] = obs.InstanceID
	} else {
		payload["instanceId"] = serviceHealthUnknown
	}
	if obs.IdentityProblem != "" {
		payload["identityProblem"] = obs.IdentityProblem
	}
	if obs.DisplayName != "" {
		payload["instanceDisplayName"] = obs.DisplayName
	}
	if !obs.StartedAt.IsZero() {
		payload["daemonStartedAt"] = obs.StartedAt.Format(time.RFC3339Nano)
		payload["processUptimeSeconds"] = obs.ObservedAt.Sub(obs.StartedAt).Seconds()
	}
	// Only reported when the window was actually readable. Emitting zero for an
	// unreadable window would be the exact confusion #5244 forbids: a covered
	// empty window and an unmeasured one must not look the same.
	if obs.WindowCoverage == serviceHealthWindowComplete {
		payload["observedUncleanRestarts"] = obs.UncleanRestarts
		if !obs.WindowStart.IsZero() {
			payload["observationWindowStart"] = obs.WindowStart.Format(time.RFC3339Nano)
		}
	}
	if obs.RecoveryInventory != nil {
		payload["recoveryInventory"] = recoveryInventoryHealthPayload(obs.RecoveryInventory)
	}
	return payload
}

// recoveryInventorySampler supplies the most recent occupancy reading.
type recoveryInventorySampler func() *readservice.RecoveryInventoryStatus

// recoveryInventoryHealthPayload renders occupancy into the health record.
// The state, not just the counts, is recorded: a reader comparing records
// months apart should not have to re-derive the high-water threshold that was
// in force when each one was written.
func recoveryInventoryHealthPayload(status *readservice.RecoveryInventoryStatus) map[string]any {
	payload := map[string]any{
		"state":            status.State,
		"used":             status.Used,
		"limit":            status.Limit,
		"unreadable":       status.Unreadable,
		"highWaterPercent": status.HighWaterPercent,
		"inventoryRoot":    status.InventoryRoot,
		"policySource":     status.PolicySource,
	}
	if status.EarliestRetainUntil != nil {
		payload["earliestRetainUntil"] = status.EarliestRetainUntil.Format(time.RFC3339Nano)
	}
	if status.Error != "" {
		payload["error"] = status.Error
	}
	return payload
}

// appendServiceHealth records one observation into the instance diagnostic log.
func appendServiceHealth(root string, identity *daemonIdentity, log *journal.InstanceLog, inventory recoveryInventorySampler, now time.Time, sinks ...func(journal.Event)) error {
	if log == nil {
		return nil
	}
	event := journal.Event{
		Time:   now,
		Type:   journal.EventServiceHealth,
		Runner: serviceHealthPayload(observeServiceHealth(root, identity, log, inventory, now)),
	}
	err := log.Append(event)
	for _, sink := range sinks {
		sink(event)
	}
	return err
}

// emitServiceHealth writes the startup record and then one per interval.
//
// Deliberately NOT a run: #5244 forbids fabricating a workflow run or holding
// an artificial six-hour task open to represent an idle instance. This is a
// plain periodic append to the instance log, so an idle daemon emits records
// while owning no run at all.
//
// now is a clock seam so the cadence is asserted deterministically rather than
// by waiting; nil means time.Now.
func emitServiceHealth(
	ctx context.Context,
	root string,
	identity *daemonIdentity,
	log *journal.InstanceLog,
	inventory recoveryInventorySampler,
	interval time.Duration,
	now func() time.Time,
	done chan<- struct{},
	sinks ...func(journal.Event),
) {
	if done != nil {
		defer close(done)
	}
	if now == nil {
		now = time.Now
	}
	// Startup record first, before any tick: an instance that is restarted more
	// often than the interval would otherwise never emit one at all.
	_ = appendServiceHealth(root, identity, log, inventory, now(), sinks...)
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// A tick already queued when cancellation won the race must not
			// produce a record for a daemon that is shutting down.
			if ctx.Err() != nil {
				return
			}
			_ = appendServiceHealth(root, identity, log, inventory, now(), sinks...)
		}
	}
}
