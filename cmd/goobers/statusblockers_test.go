package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/blockerclear"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/runner"
)

type steppedClock struct{ now time.Time }

func (c *steppedClock) Now() time.Time { return c.now }

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
	daemon1 := blockerclear.NewRecorder(layout.SchedulerDir(), firstLifetime.Now)
	daemon1.Observe("earlier-run", firstLifetime.now, nil)
	recordObservationFailure(t, layout, "stale-blocker", journal.ErrorCodeRecoveryObservationFailed, failedAt)
	daemon1.Observe("stale-blocker", failedAt, &runner.RecoveryInventoryError{Err: errors.New("recovery inventory is full")})
	recordObservationFailure(t, layout, "own-record", journal.ErrorCodeRecoveryObservationRunFailed, failedAt)
	daemon1.Observe("own-record", failedAt, errors.New("read own retained record"))

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
	daemon2 := blockerclear.NewRecorder(layout.SchedulerDir(), secondLifetime.Now)
	daemon2.Observe("next-lifetime-run", secondLifetime.now, nil)

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
	if got := operator.BlockerLine(now); got != want {
		t.Fatalf("blocker text =\n%q\nwant\n%q", got, want)
	}
	// A summary from a reader without structured blockers keeps its text.
	operator.Blockers = nil
	if got := operator.BlockerLine(now); got != "recovery_observation_failed: inventory full; review needs-changes: fix it" {
		t.Fatalf("legacy blocker text = %q", got)
	}
}
