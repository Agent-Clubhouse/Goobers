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

// TestRecoveryObservationRecorderWritesSparingly pins the write policy: the
// first success of a process, the first success after a failure, and a
// periodic refresh — not one record per stage cleanup.
func TestRecoveryObservationRecorderWritesSparingly(t *testing.T) {
	dir := instance.NewLayout(t.TempDir()).SchedulerDir()
	clock := &steppedClock{now: time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)}
	recorder := newRecoveryObservationRecorder(dir, clock.Now)
	full := errors.New("recovery inventory is full")

	steps := []struct {
		advance time.Duration
		err     error
		want    int
	}{
		{0, nil, 1},                               // first success of the process
		{time.Minute, nil, 1},                     // nothing new to say
		{time.Minute, full, 1},                    // failures live in the run journal
		{time.Minute, nil, 2},                     // first success after a failure
		{time.Minute, nil, 2},                     // nothing new again
		{recoveryObservationRefresh, nil, 3},      // periodic refresh
		{recoveryObservationRefresh / 2, nil, 3},  // within the refresh window
		{recoveryObservationRefresh / 2, full, 3}, // failure, then
		{0, nil, 4},                               // immediately cleared by the next success
	}
	for i, step := range steps {
		clock.now = clock.now.Add(step.advance)
		recorder.observe("run", step.err)
		if got := blockerClearedRecords(t, dir); len(got) != step.want {
			t.Fatalf("step %d: %d blocker.cleared records, want %d", i, len(got), step.want)
		}
	}
	last := blockerClearedRecords(t, dir)[3]
	if last.RunID != "run" || last.Runner["code"] != journal.ErrorCodeRecoveryObservationFailed || !last.Time.Equal(clock.now) {
		t.Fatalf("blocker.cleared record = %+v", last)
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

// TestStatusRecoveryObservationBlockerClearsAcrossDaemonRestart is #5404's
// regression. One daemon lifetime observes successfully, then records
// recovery_observation_failed on a run that goes on to complete — a run that
// will never observe again. Status shows that blocker once, with when it was
// observed. The next daemon lifetime's first successful observation (by a
// different run) retracts it from both the text and the JSON status.
func TestStatusRecoveryObservationBlockerClearsAcrossDaemonRestart(t *testing.T) {
	root := initScheduledDemo(t)
	layout := instance.NewLayout(root)
	firstLifetime := &steppedClock{now: time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)}
	failedAt := firstLifetime.now.Add(time.Minute)

	// Lifetime 1: an earlier success, then the failure on run "stale-blocker".
	daemon1 := newRecoveryObservationRecorder(layout.SchedulerDir(), firstLifetime.Now)
	daemon1.observe("earlier-run", nil)
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		Schema: journal.RunSchema, RunID: "stale-blocker", Gaggle: "example", Workflow: "default-implement",
		WorkflowVersion: 1, StartedAt: firstLifetime.now,
	}, nil, journal.WithClock(func() time.Time { return failedAt }))
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventError, Stage: "implement",
		Error: &journal.ErrorDetail{Code: journal.ErrorCodeRecoveryObservationFailed, Message: "observe retained recovery state: recovery inventory is full: 10 of 8 slots used"},
	}); err != nil {
		t.Fatal(err)
	}
	daemon1.observe("stale-blocker", errors.New("recovery inventory is full"))
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

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
	if strings.Count(text, journal.ErrorCodeRecoveryObservationFailed) != 1 ||
		!strings.Contains(text, "(observed "+failedAt.Format(time.RFC3339)+", ") {
		t.Fatalf("status text before restart must show the blocker once with its observed time:\n%s", text)
	}

	// Lifetime 2: a fresh process, a different run, a successful observation.
	secondLifetime := &steppedClock{now: failedAt.Add(time.Hour)}
	daemon2 := newRecoveryObservationRecorder(layout.SchedulerDir(), secondLifetime.Now)
	daemon2.observe("next-lifetime-run", nil)

	after := statusJSONRun(t, root, "stale-blocker")
	if len(after.Operator.Blockers) != 0 || len(after.Operator.PotentialBlockers) != 0 {
		t.Fatalf("blockers after a later lifetime observed successfully = %+v", after.Operator)
	}
	if after.Operator.LatestError == nil || after.Operator.LatestError.Code != journal.ErrorCodeRecoveryObservationFailed {
		t.Fatalf("the recorded error is history and must remain: %+v", after.Operator.LatestError)
	}
	code, text, stderr = runArgs(t, "status", root)
	if code != 0 {
		t.Fatalf("status code = %d, stderr = %q", code, stderr)
	}
	if strings.Contains(text, journal.ErrorCodeRecoveryObservationFailed) {
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
