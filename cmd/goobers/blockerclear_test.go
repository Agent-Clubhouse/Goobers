package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
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
	dir := instance.NewLayout(t.TempDir()).SchedulerDir()
	clock := &steppedClock{now: time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)}
	recorder := newRecoveryObservationRecorder(dir, clock.Now)
	full := &runner.RecoveryInventoryError{Err: errors.New("recovery inventory is full")}
	ownRecord := errors.New("read own retained record")

	steps := []struct {
		advance       time.Duration
		runID         string
		err           error
		instance, run int
	}{
		{0, "a", nil, 1, 0},                               // first success of the process
		{time.Minute, "a", nil, 1, 0},                     // nothing new to say
		{time.Minute, "a", full, 1, 0},                    // failures live in the run journal
		{time.Minute, "b", nil, 2, 0},                     // first success after an inventory failure
		{time.Minute, "b", nil, 2, 0},                     // nothing new again
		{time.Minute, "c", ownRecord, 2, 0},               // a run-specific failure of c
		{time.Minute, "b", nil, 2, 0},                     // another run's success says nothing about c
		{time.Minute, "c", nil, 2, 1},                     // c's own success clears c
		{time.Minute, "c", nil, 2, 1},                     // once
		{recoveryObservationRefresh, "a", nil, 3, 1},      // periodic refresh
		{recoveryObservationRefresh / 2, "a", nil, 3, 1},  // within the refresh window
		{recoveryObservationRefresh / 2, "a", full, 3, 1}, // inventory failure, then
		{time.Minute, "a", nil, 4, 1},                     // cleared by the next success after it
	}
	for i, step := range steps {
		clock.now = clock.now.Add(step.advance)
		recorder.observe(step.runID, clock.now.Add(-time.Second), step.err)
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
	dir := instance.NewLayout(t.TempDir()).SchedulerDir()
	clock := &steppedClock{now: time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)}
	recorder := newRecoveryObservationRecorder(dir, clock.Now)
	recorder.observe("warm-up", clock.now, nil) // the first-of-process clear is out of the way

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
		recorder.observe("par", clock.now.Add(-time.Second), tc.err) // branch X fails
		clock.now = clock.now.Add(time.Second)
		recorder.observe("par", branchYStarted, nil) // branch Y, started before X's failure
		if got := countClears(blockerClearedRecords(t, dir), tc.scope); got != before {
			t.Fatalf("%s: a success that started before the failure wrote %d clear(s)", tc.scope, got-before)
		}
		clock.now = clock.now.Add(time.Minute)
		laterStart := clock.now.Add(-time.Second)
		recorder.observe("par", laterStart, nil) // a later stage of the same run
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

func statusJSONRun(t *testing.T, root, runID string) statusJSONSummary {
	t.Helper()
	code, stdout, stderr := runArgs(t, "status", "--json", root)
	if code != 0 {
		t.Fatalf("status --json code = %d, stderr = %q", code, stderr)
	}
	var got statusJSONOutput
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("status JSON = %q: %v", stdout, err)
	}
	for _, run := range got.Runs {
		if run.RunID == runID {
			return run
		}
	}
	t.Fatalf("status --json omitted run %s: %s", runID, stdout)
	return statusJSONSummary{}
}

// recordObservationFailure writes a completed run whose post-cleanup
// observation failed at failedAt with code, as the runner journals it.
func recordObservationFailure(t *testing.T, layout instance.Layout, runID, code string, failedAt time.Time) {
	t.Helper()
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		Schema: journal.RunSchema, RunID: runID, Gaggle: "example", Workflow: "default-implement",
		WorkflowVersion: 1, StartedAt: failedAt.Add(-time.Minute),
	}, nil, journal.WithClock(func() time.Time { return failedAt }))
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventError, Stage: "implement",
		Error: &journal.ErrorDetail{Code: code, Message: "observe retained recovery state: recovery inventory is full: 10 of 8 slots used"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestStatusRecoveryObservationBlockerClearsAcrossDaemonRestart is #5404's
// regression. One daemon lifetime observes successfully, then records an
// inventory recovery_observation_failed on a run that goes on to complete — a
// run that will never observe again. Status shows that blocker once, with when
// it was observed. The next daemon lifetime's first successful observation (by
// a different run) retracts it from both the text and the JSON status. A
// run-specific failure on another completed run is NOT retracted by that
// success: nothing re-checked that run's own records or journal.
func TestStatusRecoveryObservationBlockerClearsAcrossDaemonRestart(t *testing.T) {
	root := initScheduledDemo(t)
	layout := instance.NewLayout(root)
	firstLifetime := &steppedClock{now: time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)}
	failedAt := firstLifetime.now.Add(time.Minute)

	// Lifetime 1: an earlier success, then an inventory failure on
	// "stale-blocker" and a run-specific one on "own-record".
	daemon1 := newRecoveryObservationRecorder(layout.SchedulerDir(), firstLifetime.Now)
	daemon1.observe("earlier-run", firstLifetime.now, nil)
	recordObservationFailure(t, layout, "stale-blocker", journal.ErrorCodeRecoveryObservationFailed, failedAt)
	daemon1.observe("stale-blocker", failedAt, &runner.RecoveryInventoryError{Err: errors.New("recovery inventory is full")})
	recordObservationFailure(t, layout, "own-record", journal.ErrorCodeRecoveryObservationRunFailed, failedAt)
	daemon1.observe("own-record", failedAt, errors.New("read own retained record"))

	before := statusJSONRun(t, root, "stale-blocker")
	if len(before.Operator.Blockers) != 1 || len(before.Operator.PotentialBlockers) != 1 {
		t.Fatalf("blockers before restart = %+v", before.Operator)
	}
	blocker := before.Operator.Blockers[0]
	if blocker.Code != journal.ErrorCodeRecoveryObservationFailed || blocker.ObservedAt == nil ||
		!blocker.ObservedAt.Equal(failedAt) || blocker.AgeMillis == nil {
		t.Fatalf("blocker before restart = %+v, want observed at %s with an age", blocker, failedAt)
	}
	code, text, stderr := runArgs(t, "status", root)
	if code != 0 {
		t.Fatalf("status code = %d, stderr = %q", code, stderr)
	}
	if strings.Count(text, journal.ErrorCodeRecoveryObservationFailed+":") != 1 ||
		!strings.Contains(text, "(observed "+failedAt.Format(time.RFC3339)+", ") {
		t.Fatalf("status text before restart must show the blocker once with its observed time:\n%s", text)
	}

	// Lifetime 2: a fresh process, a different run, a successful observation.
	secondLifetime := &steppedClock{now: failedAt.Add(time.Hour)}
	daemon2 := newRecoveryObservationRecorder(layout.SchedulerDir(), secondLifetime.Now)
	daemon2.observe("next-lifetime-run", secondLifetime.now, nil)

	after := statusJSONRun(t, root, "stale-blocker")
	if len(after.Operator.Blockers) != 0 || len(after.Operator.PotentialBlockers) != 0 {
		t.Fatalf("blockers after a later lifetime observed successfully = %+v", after.Operator)
	}
	if after.Operator.LatestError == nil || after.Operator.LatestError.Code != journal.ErrorCodeRecoveryObservationFailed {
		t.Fatalf("the recorded error is history and must remain: %+v", after.Operator.LatestError)
	}
	ownRecord := statusJSONRun(t, root, "own-record")
	if len(ownRecord.Operator.Blockers) != 1 || ownRecord.Operator.Blockers[0].Code != journal.ErrorCodeRecoveryObservationRunFailed {
		t.Fatalf("another run's success retracted a run-specific failure: %+v", ownRecord.Operator)
	}
	code, text, stderr = runArgs(t, "status", root)
	if code != 0 {
		t.Fatalf("status code = %d, stderr = %q", code, stderr)
	}
	if strings.Contains(text, journal.ErrorCodeRecoveryObservationFailed+":") {
		t.Fatalf("status text still shows the cleared blocker:\n%s", text)
	}
}

func TestStatusBlockerTextRendersObservedTimeAndAge(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	observed := now.Add(-90 * time.Minute)
	operator := readservice.OperatorRunSummary{
		PotentialBlockers: []string{"recovery_observation_failed: inventory full", "review needs-changes: fix it"},
		Blockers: []readservice.OperatorBlocker{
			{Code: journal.ErrorCodeRecoveryObservationFailed, Source: readservice.BlockerSourceError, Message: "recovery_observation_failed: inventory full", ObservedAt: &observed},
			{Code: readservice.BlockerCodeReviewNotPassed, Source: readservice.BlockerSourceReview, Message: "review needs-changes: fix it"},
		},
	}
	want := "recovery_observation_failed: inventory full (observed 2026-09-18T10:30:00Z, 1h30m0s ago); " +
		"review needs-changes: fix it (observed at an unknown time)"
	if got := statusBlockerText(operator, now); got != want {
		t.Fatalf("blocker text =\n%q\nwant\n%q", got, want)
	}
	// A summary from a reader without structured blockers keeps its text.
	operator.Blockers = nil
	if got := statusBlockerText(operator, now); got != "recovery_observation_failed: inventory full; review needs-changes: fix it" {
		t.Fatalf("legacy blocker text = %q", got)
	}
}
