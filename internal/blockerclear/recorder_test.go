package blockerclear

import (
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

type steppedClock struct{ now time.Time }

func (c *steppedClock) Now() time.Time { return c.now }

func blockerClearedRecords(t *testing.T, schedulerDir string) []journal.Event {
	t.Helper()
	events, err := journal.ReadInstanceLogAfterSeq(schedulerDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	var cleared []journal.Event
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation && event.Runner["kind"] == journal.RunnerAnnotationBlockerCleared {
			cleared = append(cleared, event)
		}
	}
	return cleared
}

func countClears(events []journal.Event, scope string) int {
	count := 0
	for _, event := range events {
		if event.Runner["scope"] == scope {
			count++
		}
	}
	return count
}

// TestRecoveryObservationRecorderWritesSparingly pins the write policy. An
// instance clear is written on the first success of a process, the first
// success after an INVENTORY failure, and a periodic refresh — not one record
// per stage cleanup. A run clear is written only when a run that failed for a
// run-specific reason later observes successfully itself.
func TestRecoveryObservationRecorderWritesSparingly(t *testing.T) {
	dir := t.TempDir()
	clock := &steppedClock{now: time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)}
	recorder := NewRecorder(dir, clock.Now)
	full := &runner.RecoveryInventoryError{Err: errors.New("recovery inventory is full")}
	ownRecord := errors.New("read own retained record")

	steps := []struct {
		advance       time.Duration
		runID         string
		err           error
		instance, run int
	}{
		{0, "a", nil, 1, 0},                 // first success of the process
		{time.Minute, "a", nil, 1, 0},       // nothing new to say
		{time.Minute, "a", full, 1, 0},      // failures live in the run journal
		{time.Minute, "b", nil, 2, 0},       // first success after an inventory failure
		{time.Minute, "b", nil, 2, 0},       // nothing new again
		{time.Minute, "c", ownRecord, 2, 0}, // a run-specific failure of c
		{time.Minute, "b", nil, 2, 0},       // another run's success says nothing about c
		{time.Minute, "c", nil, 2, 1},       // c's own success clears c
		{time.Minute, "c", nil, 2, 1},       // once
		{Refresh, "a", nil, 3, 1},           // periodic refresh
		{Refresh / 2, "a", nil, 3, 1},       // within the refresh window
		{Refresh / 2, "a", full, 3, 1},      // inventory failure, then
		{time.Minute, "a", nil, 4, 1},       // cleared by the next success after it
	}
	for i, step := range steps {
		clock.now = clock.now.Add(step.advance)
		recorder.Observe(step.runID, clock.now.Add(-time.Second), step.err)
		got := blockerClearedRecords(t, dir)
		if countClears(got, "instance") != step.instance || countClears(got, "run") != step.run {
			t.Fatalf("step %d: %d instance and %d run clears, want %d and %d", i, countClears(got, "instance"), countClears(got, "run"), step.instance, step.run)
		}
	}
	records := blockerClearedRecords(t, dir)
	var runClear journal.Event
	for _, record := range records {
		if record.Runner["scope"] == "run" {
			runClear = record
		}
	}
	if runClear.RunID != "c" || runClear.Runner["code"] != journal.ErrorCodeRecoveryObservationRunFailed {
		t.Fatalf("run clear = %+v", runClear)
	}
	last := records[len(records)-1]
	// The record carries when the observation started, not when it was
	// written: a success that read the inventory before a concurrent failure
	// must not be stamped after it.
	if last.RunID != "a" || last.Runner["code"] != journal.ErrorCodeRecoveryObservationFailed ||
		last.Runner["observedAt"] != clock.now.Add(-time.Second).Format(time.RFC3339Nano) || !last.Time.Equal(clock.now) {
		t.Fatalf("instance clear = %+v", last)
	}
}

// TestRecoveryObservationRecorderKeepsFailureForALaterSuccess is the review
// repro for parallel branches. Branch Y's observation of run "par" starts
// before branch X of the same run reports a run-specific failure, and finishes
// after it. That success is too early to retract X's failure — readers would
// (correctly) ignore a clear stamped before the failure — so it must not
// consume the pending failure either; otherwise the run's next, genuinely
// later success writes no clear and the blocker sticks. The same holds for an
// inventory failure and the instance clear.
func TestRecoveryObservationRecorderKeepsFailureForALaterSuccess(t *testing.T) {
	dir := t.TempDir()
	clock := &steppedClock{now: time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)}
	recorder := NewRecorder(dir, clock.Now)
	recorder.Observe("warm-up", clock.now, nil) // the first-of-process clear is out of the way

	for _, tc := range []struct {
		scope string
		err   error
	}{
		{"run", errors.New("append own recovery observations")},
		{"instance", &runner.RecoveryInventoryError{Err: errors.New("recovery inventory is full")}},
	} {
		before := countClears(blockerClearedRecords(t, dir), tc.scope)
		branchYStarted := clock.now.Add(time.Second)
		clock.now = clock.now.Add(2 * time.Second)
		recorder.Observe("par", clock.now.Add(-time.Second), tc.err) // branch X fails
		clock.now = clock.now.Add(time.Second)
		recorder.Observe("par", branchYStarted, nil) // branch Y, started before X's failure
		if got := countClears(blockerClearedRecords(t, dir), tc.scope); got != before {
			t.Fatalf("%s: a success that started before the failure wrote %d clear(s)", tc.scope, got-before)
		}
		clock.now = clock.now.Add(time.Minute)
		laterStart := clock.now.Add(-time.Second)
		recorder.Observe("par", laterStart, nil) // a later stage of the same run
		records := blockerClearedRecords(t, dir)
		if got := countClears(records, tc.scope); got != before+1 {
			t.Fatalf("%s: the later success wrote %d clear(s), want 1", tc.scope, got-before)
		}
		last := records[len(records)-1]
		if last.Runner["scope"] != tc.scope || last.Runner["observedAt"] != laterStart.Format(time.RFC3339Nano) {
			t.Fatalf("%s: clear = %+v", tc.scope, last)
		}
	}
}
