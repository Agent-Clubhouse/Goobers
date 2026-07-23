package readservice

import (
	"context"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// TestStageAttemptsClosesOpenAttemptAtRunTermination is the DASH-20 regression
// guard: a gate whose evaluation errors terminally opens an attempt but emits
// no stage.finished (and its error is not an executor_error), so the attempt
// used to project as permanently "running". A terminal run must close it.
func TestStageAttemptsClosesOpenAttemptAtRunTermination(t *testing.T) {
	service, layout, machine := fixtureService(t)
	run, clock := createFixtureRun(
		t, layout, machine, "run-gate-error", machine.Def.Name, "goobers",
		time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC),
		journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	emit := func(e journal.Event) {
		t.Helper()
		clock.advance(time.Second)
		if err := run.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	emit(journal.Event{Type: journal.EventStageStarted, Stage: "review", Attempt: 1})
	emit(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)})
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	list, err := service.StageAttempts(context.Background(), "run-gate-error", "review")
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(list.Attempts))
	}
	attempt := list.Attempts[0]
	if attempt.Status != string(apiv1.ResultFailure) {
		t.Fatalf("open attempt at run termination status = %q, want %q (must not stay running)", attempt.Status, apiv1.ResultFailure)
	}
	if attempt.FinishedSeq == 0 || attempt.FinishedAt == nil {
		t.Fatalf("attempt not closed at run termination: finishedSeq=%d finishedAt=%v", attempt.FinishedSeq, attempt.FinishedAt)
	}
}
