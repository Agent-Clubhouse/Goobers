// Package blockerclear journals the evidence that retracts run blockers whose
// condition has since been observed healthy (#5404): blocker.cleared records in
// the instance journal, which the read service folds to stop listing a blocker
// recorded before them.
package blockerclear

import (
	"sync"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

// Refresh bounds how long a recovery_observation_failed
// blocker recorded by ANOTHER process (a foreground `goobers run`, a worker)
// can outlive the condition while this process keeps observing successfully.
// Failures this process sees itself are cleared by its very next success.
const Refresh = 15 * time.Minute

// Recorder journals the evidence that retracts recovery
// observation run blockers (#5404).
//
// A failed post-cleanup observation is recorded in the run's own journal and
// read-model row, and nothing used to retract it: a terminal run never
// observes again, so the blocker stayed on `goobers status` after the
// inventory became readable — once per affected run, across every later
// daemon lifetime. This recorder writes blocker.cleared records to the
// instance journal, each carrying when the successful observation STARTED,
// and the read service drops blockers recorded before that time. There are
// two scopes, because the observation can fail in two ways:
//
//   - recovery_observation_failed: the shared inventory could not be read.
//     Any run's later successful observation reads that same inventory, so
//     an "instance" clear is written on the first success of a process (so a
//     blocker from an earlier daemon lifetime is cleared by the next one), on
//     the first success after an inventory failure this process saw, and
//     otherwise at most once per Refresh.
//   - recovery_observation_run_failed: the run's own records, its count or
//     ownership checks, or its journal append failed. Another run's success
//     proves nothing about those, so a "run" clear is written only when the
//     SAME run later observes successfully after a failure this process saw.
//     A run that never observes again keeps its blocker.
//
// Failures are never written here — the run journal already holds them — so a
// missed or failed write only leaves a blocker visible longer; it can never
// hide one.
type Recorder struct {
	schedulerDir string
	now          func() time.Time
	refresh      time.Duration

	mu          sync.Mutex
	written     bool
	lastWritten time.Time
	// inventoryFailedAt and failedRuns hold when each still-uncleared failure
	// was reported. A success clears one only if the success STARTED after
	// it: with parallel branches, a success that read the inventory (or the
	// run's records) before a sibling's failure is too early to retract that
	// failure, and consuming the pending entry on it would leave no clear for
	// the run's next, genuinely later, success to write.
	inventoryFailedAt time.Time
	failedRuns        map[string]time.Time
}

// NewRecorder returns a Recorder writing to the instance journal in
// schedulerDir. A nil now uses time.Now. Each Recorder is one process
// lifetime's view; production code uses Reporter.
func NewRecorder(schedulerDir string, now func() time.Time) *Recorder {
	if now == nil {
		now = time.Now
	}
	return &Recorder{
		schedulerDir: schedulerDir,
		now:          now,
		refresh:      Refresh,
		failedRuns:   make(map[string]time.Time),
	}
}

// recorders keeps one Recorder per instance per process, so the per-gaggle
// runners a daemon composes (and recomposes on reload) share one notion of
// "already cleared in this lifetime".
var recorders sync.Map

// Reporter is the runner.Config.RecoveryObserved hook for the instance whose
// instance journal lives in schedulerDir.
func Reporter(schedulerDir string) func(string, time.Time, error) {
	recorder, _ := recorders.LoadOrStore(schedulerDir, NewRecorder(schedulerDir, nil))
	return recorder.(*Recorder).Observe
}

// Observe records one post-cleanup observation outcome of run runID, started
// at observedAt, and journals any clear it makes due.
func (r *Recorder) Observe(runID string, observedAt time.Time, err error) {
	claim := r.claimWrites(runID, observedAt, err)
	if !claim.instance && !claim.run {
		return
	}
	var events []journal.Event
	if claim.instance {
		events = append(events, blockerClearedEvent(runID, journal.ErrorCodeRecoveryObservationFailed, "instance", observedAt))
	}
	if claim.run {
		events = append(events, blockerClearedEvent(runID, journal.ErrorCodeRecoveryObservationRunFailed, "run", observedAt))
	}
	writeErr := appendInstanceEvents(r.schedulerDir, r.now, events)
	r.mu.Lock()
	defer r.mu.Unlock()
	if writeErr != nil {
		// Restore the failures the write was meant to clear so the next
		// success after them tries again.
		r.inventoryFailedAt = latest(r.inventoryFailedAt, claim.inventoryFailedAt)
		if !claim.runFailedAt.IsZero() {
			r.failedRuns[runID] = latest(r.failedRuns[runID], claim.runFailedAt)
		}
		return
	}
	if claim.instance {
		r.written = true
		r.lastWritten = r.now()
	}
}

// clearClaim is what one outcome must journal, and the pending failures it
// consumed (zero when none), so a failed write can restore them.
type clearClaim struct {
	instance, run                  bool
	inventoryFailedAt, runFailedAt time.Time
}

// claimWrites decides under the lock which clears this outcome must journal,
// so the journal write itself happens without holding it: the instance-log
// lock can take a while, and the hook runs on a stage's cleanup path.
func (r *Recorder) claimWrites(runID string, observedAt time.Time, err error) clearClaim {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		// The failure is journaled after this report, so the report time is
		// a lower bound on the time readers compare a clear against.
		now := r.now()
		if runner.IsRecoveryInventoryError(err) {
			r.inventoryFailedAt = latest(r.inventoryFailedAt, now)
		} else {
			r.failedRuns[runID] = latest(r.failedRuns[runID], now)
		}
		return clearClaim{}
	}
	var claim clearClaim
	if failedAt, failed := r.failedRuns[runID]; failed && observedAt.After(failedAt) {
		delete(r.failedRuns, runID)
		claim.run, claim.runFailedAt = true, failedAt
	}
	if !r.inventoryFailedAt.IsZero() && observedAt.After(r.inventoryFailedAt) {
		claim.instance, claim.inventoryFailedAt = true, r.inventoryFailedAt
		r.inventoryFailedAt = time.Time{}
	}
	if !r.written || r.now().Sub(r.lastWritten) >= r.refresh {
		claim.instance = true
	}
	return claim
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func blockerClearedEvent(runID, code, scope string, observedAt time.Time) journal.Event {
	return journal.Event{
		Type:  journal.EventRunnerAnnotation,
		RunID: runID,
		Runner: map[string]any{
			"kind":       journal.RunnerAnnotationBlockerCleared,
			"code":       code,
			"scope":      scope,
			"observedAt": observedAt.UTC().Format(time.RFC3339Nano),
		},
	}
}

// appendInstanceEvents opens the instance journal for one batch of appends,
// as other out-of-scheduler writers do.
func appendInstanceEvents(dir string, now func() time.Time, events []journal.Event) error {
	log, _, err := journal.OpenInstanceLog(dir, journal.WithClock(now))
	if err != nil {
		return err
	}
	var appendErr error
	for _, event := range events {
		if appendErr = log.Append(event); appendErr != nil {
			break
		}
	}
	if closeErr := log.Close(); appendErr == nil {
		appendErr = closeErr
	}
	return appendErr
}
