package readservice

import (
	"context"
	"fmt"
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

func TestStageAttemptsDistinguishRepassesFromRetries(t *testing.T) {
	service, layout, machine := fixtureService(t)
	run, clock := createFixtureRun(
		t, layout, machine, "run-repasses", machine.Def.Name, "goobers",
		time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC),
		journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	appendEvent := func(event journal.Event) {
		t.Helper()
		clock.advance(time.Second)
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	recordArtifact := func(visit, attempt int, class journal.AttemptClass) journal.Ref {
		t.Helper()
		clock.advance(time.Second)
		ref, err := run.RecordStageArtifact(
			"implement", attempt, class, fmt.Sprintf("visit-%d.json", visit),
			[]byte(fmt.Sprintf(`{"visit":%d}`, visit)),
		)
		if err != nil {
			t.Fatal(err)
		}
		ref.MediaType = "application/json"
		return ref
	}

	appendEvent(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1})
	appendEvent(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 1,
		Status: string(apiv1.ResultFailure),
		Error:  &journal.ErrorDetail{Code: "visit_1_failed"},
	})
	appendEvent(journal.Event{
		Type: journal.EventStageStarted, Stage: "implement", Attempt: 2,
		AttemptClass: journal.AttemptPolicy,
	})
	firstArtifact := recordArtifact(1, 2, journal.AttemptPolicy)
	appendEvent(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 2,
		AttemptClass: journal.AttemptPolicy, Status: string(apiv1.ResultSuccess),
		Outputs: map[string]any{"visit": 1}, Artifacts: []journal.Ref{firstArtifact},
	})
	appendEvent(journal.Event{
		Type: journal.EventGateEvaluated, Gate: "review",
		Verdict: "needs-changes", Target: "implement",
	})

	appendEvent(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1})
	appendEvent(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 1,
		Status: string(apiv1.ResultFailure),
		Error:  &journal.ErrorDetail{Code: "visit_2_failed"},
	})
	appendEvent(journal.Event{
		Type: journal.EventStageStarted, Stage: "implement", Attempt: 2,
		AttemptClass: journal.AttemptInfra,
	})
	secondArtifact := recordArtifact(2, 2, journal.AttemptInfra)
	appendEvent(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 2,
		AttemptClass: journal.AttemptInfra, Status: string(apiv1.ResultSuccess),
		Outputs: map[string]any{"visit": 2}, Artifacts: []journal.Ref{secondArtifact},
	})
	appendEvent(journal.Event{
		Type: journal.EventGateEvaluated, Gate: "review",
		Verdict: "needs-changes", Target: "implement",
	})

	appendEvent(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1})
	live, err := service.StageAttempts(context.Background(), "run-repasses", "implement")
	if err != nil {
		t.Fatal(err)
	}
	if len(live.Attempts) != 5 || live.Attempts[4].Status != "running" {
		t.Fatalf("live attempts = %+v", live.Attempts)
	}
	liveID := live.Attempts[4].ID

	thirdArtifact := recordArtifact(3, 1, "")
	appendEvent(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 1,
		Status: string(apiv1.ResultSuccess), Outputs: map[string]any{"visit": 3},
		Artifacts: []journal.Ref{thirdArtifact},
	})
	finishFixtureRun(t, run, clock, journal.PhaseCompleted)

	got, err := service.StageAttempts(context.Background(), "run-repasses", "implement")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Attempts) != 5 {
		t.Fatalf("attempts = %+v", got.Attempts)
	}
	wantVisits := []int{1, 1, 2, 2, 3}
	wantNumbers := []int{1, 2, 1, 2, 1}
	wantClasses := []string{"initial", "policy", "initial", "infra", "initial"}
	ids := make(map[string]struct{}, len(got.Attempts))
	for i, attempt := range got.Attempts {
		if attempt.ID == "" || attempt.Visit != wantVisits[i] ||
			attempt.Number != wantNumbers[i] || attempt.Class != wantClasses[i] {
			t.Fatalf("attempt %d = %+v", i, attempt)
		}
		if _, exists := ids[attempt.ID]; exists {
			t.Fatalf("duplicate attempt ID %q", attempt.ID)
		}
		ids[attempt.ID] = struct{}{}
	}
	if got.Attempts[4].ID != liveID {
		t.Fatalf("completed traversal ID = %q, want live ID %q", got.Attempts[4].ID, liveID)
	}
	if got.Attempts[0].Error == nil || got.Attempts[0].Error.Code != "visit_1_failed" ||
		got.Attempts[2].Error == nil || got.Attempts[2].Error.Code != "visit_2_failed" {
		t.Fatalf("repass errors = %+v", got.Attempts)
	}
	for i, attemptIndex := range []int{1, 3, 4} {
		attempt := got.Attempts[attemptIndex]
		visit := i + 1
		if attempt.Outputs["visit"] != float64(visit) ||
			len(attempt.Artifacts) != 1 ||
			attempt.Artifacts[0].Name != fmt.Sprintf("visit-%d.json", visit) {
			t.Fatalf("visit %d payload = %+v", visit, attempt)
		}
	}
}

func TestStageAttemptsKeepInterruptedHumanRerunInOneVisit(t *testing.T) {
	service, layout, machine := fixtureService(t)
	run, clock := createFixtureRun(
		t, layout, machine, "run-interrupted-human-rerun", machine.Def.Name, "goobers",
		time.Date(2026, 7, 24, 13, 0, 0, 0, time.UTC),
		journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	appendEvent := func(event journal.Event) {
		t.Helper()
		clock.advance(time.Second)
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}

	appendEvent(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1})
	appendEvent(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 1,
		Status: string(apiv1.ResultSuccess),
	})
	appendEvent(journal.Event{
		Type: journal.EventStageRerunRequested, Stage: "implement", Attempt: 2,
		AttemptClass: journal.AttemptHuman,
	})
	appendEvent(journal.Event{
		Type: journal.EventStageStarted, Stage: "implement", Attempt: 2,
		AttemptClass: journal.AttemptHuman,
	})
	appendEvent(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 2,
		AttemptClass: journal.AttemptHuman, Status: string(apiv1.ResultFailure),
		Error:  &journal.ErrorDetail{Code: "interrupted"},
		Runner: map[string]any{"interruptedAttempt": true},
	})
	appendEvent(journal.Event{
		Type: journal.EventStageStarted, Stage: "implement", Attempt: 3,
		AttemptClass: journal.AttemptHuman,
	})
	appendEvent(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 3,
		AttemptClass: journal.AttemptHuman, Status: string(apiv1.ResultSuccess),
	})
	appendEvent(journal.Event{
		Type: journal.EventStageRerunRequested, Stage: "implement", Attempt: 4,
		AttemptClass: journal.AttemptHuman,
	})
	appendEvent(journal.Event{
		Type: journal.EventStageStarted, Stage: "implement", Attempt: 4,
		AttemptClass: journal.AttemptHuman,
	})
	appendEvent(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 4,
		AttemptClass: journal.AttemptHuman, Status: string(apiv1.ResultSuccess),
	})
	finishFixtureRun(t, run, clock, journal.PhaseCompleted)

	got, err := service.StageAttempts(context.Background(), "run-interrupted-human-rerun", "implement")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Attempts) != 4 {
		t.Fatalf("attempts = %+v", got.Attempts)
	}
	wantVisits := []int{1, 2, 2, 3}
	for i, attempt := range got.Attempts {
		if attempt.Visit != wantVisits[i] || attempt.Number != i+1 {
			t.Fatalf("attempt %d = %+v", i, attempt)
		}
	}
}

func TestStageAttemptsGroupLegacyHumanRetries(t *testing.T) {
	service, layout, machine := fixtureService(t)
	run, clock := createFixtureRun(
		t, layout, machine, "run-legacy-human-rerun", machine.Def.Name, "goobers",
		time.Date(2026, 7, 24, 14, 0, 0, 0, time.UTC),
		journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	for _, attempt := range []int{2, 3} {
		clock.advance(time.Second)
		if err := run.Append(journal.Event{
			Type: journal.EventStageStarted, Stage: "implement", Attempt: attempt,
			AttemptClass: journal.AttemptHuman,
		}); err != nil {
			t.Fatal(err)
		}
		clock.advance(time.Second)
		if err := run.Append(journal.Event{
			Type: journal.EventStageFinished, Stage: "implement", Attempt: attempt,
			AttemptClass: journal.AttemptHuman, Status: string(apiv1.ResultSuccess),
		}); err != nil {
			t.Fatal(err)
		}
	}
	finishFixtureRun(t, run, clock, journal.PhaseCompleted)

	got, err := service.StageAttempts(context.Background(), "run-legacy-human-rerun", "implement")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Attempts) != 2 || got.Attempts[0].Visit != 1 || got.Attempts[1].Visit != 1 {
		t.Fatalf("legacy human attempts = %+v", got.Attempts)
	}
}

func TestStageAttemptsIdentifyLegacyFinishWithoutStart(t *testing.T) {
	service, layout, machine := fixtureService(t)
	run, clock := createFixtureRun(
		t, layout, machine, "run-legacy-finish", machine.Def.Name, "goobers",
		time.Date(2026, 7, 24, 13, 0, 0, 0, time.UTC),
		journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	clock.advance(time.Second)
	if err := run.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 1,
		Status: string(apiv1.ResultSuccess),
	}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := service.StageAttempts(context.Background(), "run-legacy-finish", "implement")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Attempts) != 1 || got.Attempts[0].ID == "" ||
		got.Attempts[0].Visit != 1 || got.Attempts[0].StartedSeq != 0 ||
		got.Attempts[0].FinishedSeq == 0 {
		t.Fatalf("legacy attempt = %+v", got.Attempts)
	}
}
