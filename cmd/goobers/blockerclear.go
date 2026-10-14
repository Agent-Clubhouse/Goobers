package main

import (
	"sync"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

// recoveryObservationRefresh bounds how long a recovery_observation_failed
// blocker recorded by ANOTHER process (a foreground `goobers run`, a worker)
// can outlive the condition while this process keeps observing successfully.
// Failures this process sees itself are cleared by its very next success.
const recoveryObservationRefresh = 15 * time.Minute

// recoveryObservationRecorder journals the evidence that retracts recovery
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
//     otherwise at most once per recoveryObservationRefresh.
//   - recovery_observation_run_failed: the run's own records, its count or
//     ownership checks, or its journal append failed. Another run's success
//     proves nothing about those, so a "run" clear is written only when the
//     SAME run later observes successfully after a failure this process saw.
//     A run that never observes again keeps its blocker.
//
// Failures are never written here — the run journal already holds them — so a
// missed or failed write only leaves a blocker visible longer; it can never
// hide one.
type recoveryObservationRecorder struct {
	schedulerDir string
	now          func() time.Time
	refresh      time.Duration

	mu          sync.Mutex
	written     bool
	lastWritten time.Time
	failedSince bool
	failedRuns  map[string]struct{}
}

func newRecoveryObservationRecorder(schedulerDir string, now func() time.Time) *recoveryObservationRecorder {
	if now == nil {
		now = time.Now
	}
	return &recoveryObservationRecorder{
		schedulerDir: schedulerDir,
		now:          now,
		refresh:      recoveryObservationRefresh,
		failedRuns:   make(map[string]struct{}),
	}
}

// recoveryObservationRecorders keeps one recorder per instance per process, so
// the per-gaggle runners a daemon composes (and recomposes on reload) share
// one notion of "already cleared in this lifetime".
var recoveryObservationRecorders sync.Map

// recoveryObservationReporter is the runner.Config.RecoveryObserved hook for
// the instance at l.
func recoveryObservationReporter(l instance.Layout) func(string, time.Time, error) {
	dir := l.SchedulerDir()
	recorder, _ := recoveryObservationRecorders.LoadOrStore(dir, newRecoveryObservationRecorder(dir, nil))
	return recorder.(*recoveryObservationRecorder).observe
}

func (r *recoveryObservationRecorder) observe(runID string, observedAt time.Time, err error) {
	instanceClear, runClear := r.claimWrites(runID, err)
	if !instanceClear && !runClear {
		return
	}
	var events []journal.Event
	if instanceClear {
		events = append(events, blockerClearedEvent(runID, journal.ErrorCodeRecoveryObservationFailed, "instance", observedAt))
	}
	if runClear {
		events = append(events, blockerClearedEvent(runID, journal.ErrorCodeRecoveryObservationRunFailed, "run", observedAt))
	}
	writeErr := appendInstanceEvents(r.schedulerDir, r.now, events)
	r.mu.Lock()
	defer r.mu.Unlock()
	if writeErr != nil {
		// Restore what the write was meant to clear so the next success of
		// the same kind tries again.
		r.failedSince = r.failedSince || instanceClear
		if runClear {
			r.failedRuns[runID] = struct{}{}
		}
		return
	}
	if instanceClear {
		r.written = true
		r.lastWritten = r.now()
	}
}

// claimWrites decides under the lock which clears this outcome must journal,
// so the journal write itself happens without holding it: the instance-log
// lock can take a while, and the hook runs on a stage's cleanup path.
func (r *recoveryObservationRecorder) claimWrites(runID string, err error) (instanceClear, runClear bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		if runner.IsRecoveryInventoryError(err) {
			r.failedSince = true
		} else {
			r.failedRuns[runID] = struct{}{}
		}
		return false, false
	}
	if _, failed := r.failedRuns[runID]; failed {
		delete(r.failedRuns, runID)
		runClear = true
	}
	if !r.written || r.failedSince || r.now().Sub(r.lastWritten) >= r.refresh {
		r.failedSince = false
		instanceClear = true
	}
	return instanceClear, runClear
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
