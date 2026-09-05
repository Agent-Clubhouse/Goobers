package readservice

import (
	"context"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

// TestFailedRunProjectsTerminalReason proves #4246: the terminal-cause
// projection no longer short-circuits on any non-escalated phase. A failed run
// whose last stage.finished carried a coded error reports that code as its
// terminal reason on BOTH the list summary and the run detail, so an operator
// never has to read raw journal events for a reason the journal recorded.
func TestFailedRunProjectsTerminalReason(t *testing.T) {
	service, layout, machine := fixtureService(t)
	run, clock := createFixtureRun(
		t, layout, machine, "run-failed-reason", machine.Def.Name, "goobers",
		time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC),
		journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	clock.advance(time.Second)
	if err := run.Append(journal.Event{
		Type: journal.EventStageStarted, Stage: "implement", Attempt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Second)
	if err := run.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 1,
		Status: string(apiv1.ResultFailure),
		Error:  &journal.ErrorDetail{Code: "harness.crash"},
	}); err != nil {
		t.Fatal(err)
	}
	finishFixtureRun(t, run, clock, journal.PhaseFailed)

	detail, err := service.GetRun(context.Background(), "run-failed-reason")
	if err != nil {
		t.Fatal(err)
	}
	if detail.TerminalReason != "harness.crash" {
		t.Fatalf("detail terminal reason = %q, want harness.crash", detail.TerminalReason)
	}
	if detail.TerminalCause == nil ||
		detail.TerminalCause.TerminalReason != "harness.crash" ||
		detail.TerminalCause.Selector.Kind != "stage" ||
		detail.TerminalCause.Selector.Name != "implement" {
		t.Fatalf("detail terminal cause = %+v", detail.TerminalCause)
	}
	if detail.TerminalCause.CausalEventSeq == 0 {
		t.Error("terminal cause carries no causal event seq")
	}
	// Escalation stays escalated-only so consumers keyed on it keep meaning.
	if detail.Escalation != nil {
		t.Fatalf("failed run escalation = %+v, want nil", detail.Escalation)
	}

	list, err := service.ListRuns(context.Background(), RunListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, summary := range list.Runs {
		if summary.ID != "run-failed-reason" {
			continue
		}
		found = true
		if summary.TerminalReason != "harness.crash" {
			t.Fatalf("summary terminal reason = %q, want harness.crash", summary.TerminalReason)
		}
	}
	if !found {
		t.Fatal("failed run missing from the list page")
	}
}

// TestAbortedRunProjectsTerminalReason pins the second phase #4246 covers: an
// aborted run reports the recorded reason rather than only "aborted".
func TestAbortedRunProjectsTerminalReason(t *testing.T) {
	service, layout, machine := fixtureService(t)
	run, clock := createFixtureRun(
		t, layout, machine, "run-aborted-reason", machine.Def.Name, "goobers",
		time.Date(2026, 9, 4, 13, 0, 0, 0, time.UTC),
		journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	clock.advance(time.Second)
	if err := run.Append(journal.Event{
		Type: journal.EventStageStarted, Stage: "implement", Attempt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Second)
	if err := run.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 1,
		Status: string(apiv1.ResultFailure),
		Error:  &journal.ErrorDetail{Code: "run.aborted", Message: "operator aborted the run"},
	}); err != nil {
		t.Fatal(err)
	}
	finishFixtureRun(t, run, clock, journal.PhaseAborted)

	detail, err := service.GetRun(context.Background(), "run-aborted-reason")
	if err != nil {
		t.Fatal(err)
	}
	if detail.TerminalReason != "operator aborted the run" {
		t.Fatalf("detail terminal reason = %q", detail.TerminalReason)
	}
	if detail.TerminalCause == nil || detail.TerminalCause.CausalEventSeq == 0 {
		t.Fatalf("aborted run terminal cause = %+v", detail.TerminalCause)
	}
}

// TestCompletedRunHasNoTerminalReason keeps the projection honest at the other
// end: a completed run has no terminal cause to report, so the field stays
// empty rather than reporting an incidental earlier error as the outcome.
func TestCompletedRunHasNoTerminalReason(t *testing.T) {
	service, layout, machine := fixtureService(t)
	run, clock := createFixtureRun(
		t, layout, machine, "run-completed-reason", machine.Def.Name, "goobers",
		time.Date(2026, 9, 4, 14, 0, 0, 0, time.UTC),
		journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	appendFixtureStageAttempt(t, run, clock, string(apiv1.ResultSuccess))
	finishFixtureRun(t, run, clock, journal.PhaseCompleted)

	detail, err := service.GetRun(context.Background(), "run-completed-reason")
	if err != nil {
		t.Fatal(err)
	}
	if detail.TerminalReason != "" || detail.TerminalCause != nil {
		t.Fatalf("completed run terminal reason = %q, cause = %+v",
			detail.TerminalReason, detail.TerminalCause)
	}
}

// TestReadModelSummaryDegradesToLastRecordedError pins the read-model list
// path's honest degradation: the row stores no terminal-cause column, so a
// non-completed terminal run reports the last error the projection did store
// rather than no reason at all, and a completed run reports none.
func TestReadModelSummaryDegradesToLastRecordedError(t *testing.T) {
	observedAt := time.Date(2026, 9, 4, 15, 0, 0, 0, time.UTC)
	failed := readmodel.RunRow{
		RunID: "run-failed", Phase: journal.PhaseFailed, Terminal: true,
		Operator: readmodel.OperatorFacts{
			LatestError: &journal.ErrorDetail{Code: "harness.crash"},
		},
	}
	if got := summaryFromReadModel(failed, observedAt).TerminalReason; got != "harness.crash" {
		t.Fatalf("failed row terminal reason = %q, want harness.crash", got)
	}

	completed := failed
	completed.Phase = journal.PhaseCompleted
	if got := summaryFromReadModel(completed, observedAt).TerminalReason; got != "" {
		t.Fatalf("completed row terminal reason = %q, want empty", got)
	}
}
