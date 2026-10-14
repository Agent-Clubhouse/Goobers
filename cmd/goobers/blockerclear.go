package main

import (
	"sync"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

// recoveryObservationRefresh bounds how long a recovery_observation_failed
// blocker recorded by ANOTHER process (a foreground `goobers run`, a worker)
// can outlive the condition while this process keeps observing successfully.
// Failures this process sees itself are cleared by its very next success.
const recoveryObservationRefresh = 15 * time.Minute

// recoveryObservationRecorder journals the instance-level evidence that
// retracts recovery_observation_failed run blockers (#5404).
//
// A failed post-cleanup observation is recorded in the run's own journal and
// read-model row, and nothing used to retract it: a terminal run never
// observes again, so the blocker stayed on `goobers status` after the
// inventory became readable — once per affected run, across every later
// daemon lifetime. The observation, however, reads the shared recovery
// inventory, so any later successful observation of the same kind proves the
// condition has cleared. This recorder writes that proof to the instance
// journal as a blocker.cleared annotation, and the read service drops every
// recovery_observation_failed blocker observed before it.
//
// It writes sparingly rather than per stage: on the first success of a
// process (so a blocker from an earlier daemon lifetime is cleared by the
// next one), on the first success after a failure this process observed, and
// otherwise at most once per recoveryObservationRefresh. Failures are never
// written here — the run journal already holds them — so a missed or failed
// write only leaves a blocker visible longer; it can never hide one.
type recoveryObservationRecorder struct {
	schedulerDir string
	now          func() time.Time
	refresh      time.Duration

	mu          sync.Mutex
	written     bool
	lastWritten time.Time
	failedSince bool
}

func newRecoveryObservationRecorder(schedulerDir string, now func() time.Time) *recoveryObservationRecorder {
	if now == nil {
		now = time.Now
	}
	return &recoveryObservationRecorder{
		schedulerDir: schedulerDir,
		now:          now,
		refresh:      recoveryObservationRefresh,
	}
}

// recoveryObservationRecorders keeps one recorder per instance per process, so
// the per-gaggle runners a daemon composes (and recomposes on reload) share
// one notion of "already cleared in this lifetime".
var recoveryObservationRecorders sync.Map

// recoveryObservationReporter is the runner.Config.RecoveryObserved hook for
// the instance at l.
func recoveryObservationReporter(l instance.Layout) func(string, error) {
	dir := l.SchedulerDir()
	recorder, _ := recoveryObservationRecorders.LoadOrStore(dir, newRecoveryObservationRecorder(dir, nil))
	return recorder.(*recoveryObservationRecorder).observe
}

func (r *recoveryObservationRecorder) observe(runID string, err error) {
	if !r.claimWrite(err) {
		return
	}
	writeErr := appendInstanceEvent(r.schedulerDir, r.now, journal.Event{
		Type:  journal.EventRunnerAnnotation,
		RunID: runID,
		Runner: map[string]any{
			"kind": journal.RunnerAnnotationBlockerCleared,
			"code": journal.ErrorCodeRecoveryObservationFailed,
		},
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	if writeErr != nil {
		// Leave the state as it was so the next success tries again.
		r.failedSince = true
		return
	}
	r.written = true
	r.lastWritten = r.now()
}

// claimWrite decides under the lock whether this outcome must be journaled,
// so the journal write itself happens without holding it: the instance-log
// lock can take a while, and the hook runs on a stage's cleanup path.
func (r *recoveryObservationRecorder) claimWrite(err error) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.failedSince = true
		return false
	}
	if r.written && !r.failedSince && r.now().Sub(r.lastWritten) < r.refresh {
		return false
	}
	r.failedSince = false
	return true
}

// appendInstanceEvent opens the instance journal for one append, as other
// out-of-scheduler writers do; now stamps the event.
func appendInstanceEvent(dir string, now func() time.Time, event journal.Event) error {
	log, _, err := journal.OpenInstanceLog(dir, journal.WithClock(now))
	if err != nil {
		return err
	}
	appendErr := log.Append(event)
	if closeErr := log.Close(); appendErr == nil {
		appendErr = closeErr
	}
	return appendErr
}
