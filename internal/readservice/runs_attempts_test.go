package readservice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

// TestAttachStageAttemptModelsMatchesTraversalAndAttempt (issue #1550) proves
// the requested/selected model is correlated to the right attempt by durable
// traversal (Visit) and attempt number, scoped to the requested stage — not
// just attempt number, which repeats across repass visits and across stages.
func TestAttachStageAttemptModelsMatchesTraversalAndAttempt(t *testing.T) {
	attempts := []StageAttempt{
		{Visit: 1, Number: 1},
		{Visit: 2, Number: 1},
	}
	attachStageAttemptModels(attempts, "implement", []rollup.StageAttempt{
		{Stage: "implement", Traversal: 1, Attempt: 1, Model: "auto"},
		{Stage: "implement", Traversal: 2, Attempt: 1, Model: "gpt-5.4"},
		{Stage: "other-stage", Traversal: 1, Attempt: 1, Model: "should-not-match"},
	})
	if attempts[0].Model != "auto" {
		t.Fatalf("visit 1 model = %q, want auto", attempts[0].Model)
	}
	if attempts[1].Model != "gpt-5.4" {
		t.Fatalf("visit 2 model = %q, want gpt-5.4", attempts[1].Model)
	}
}

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

// TestStageAttemptsCarryPlacementProvenance: a runner.placement event journaled
// on the stage-attempt path (goobernetes-architecture.md §7) surfaces as the
// attempt's Placement, correlated per attempt — each attempt gets its OWN
// placement, which is exactly the fresh-pod observer the smoke reads (§11
// item 6).
func TestStageAttemptsCarryPlacementProvenance(t *testing.T) {
	service, layout, machine := fixtureService(t)
	run, clock := createFixtureRun(
		t, layout, machine, "run-placement", machine.Def.Name, "goobers",
		time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC),
		journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	emit := func(event journal.Event) {
		t.Helper()
		clock.advance(time.Second)
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	queuedAt := time.Date(2026, 8, 22, 12, 0, 30, 0, time.UTC)
	podStartedAt := queuedAt.Add(9 * time.Second)

	emit(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1})
	// The self attempt knows its own hostname and NOTHING about a cluster
	// node: node stays absent rather than borrowing the hostname, which inside
	// a pod would be the pod name (#3515 finding 4).
	emit(journal.PlacementEvent("implement", 1, "", journal.Placement{
		Runner: journal.PlacementRunnerSelf, OS: "linux", Host: "daemon-host",
	}))
	emit(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 1,
		Status: string(apiv1.ResultFailure),
		Error:  &journal.ErrorDetail{Code: "attempt_1_failed"},
	})
	emit(journal.Event{
		Type: journal.EventStageStarted, Stage: "implement", Attempt: 2,
		AttemptClass: journal.AttemptPolicy,
	})
	emit(journal.PlacementEvent("implement", 2, journal.AttemptPolicy, journal.Placement{
		Runner: "linux-large", OS: "linux", Node: "aks-linux-0001",
		Image: "ghcr.io/goobers/goobers-base:v0.2.0", Pod: "goobers-stage-implement-4x2vq",
		Host: "goobers-stage-implement-4x2vq", QueuedAt: &queuedAt, PodStartedAt: &podStartedAt,
	}))
	emit(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 2,
		AttemptClass: journal.AttemptPolicy, Status: string(apiv1.ResultSuccess),
	})
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := service.StageAttempts(context.Background(), "run-placement", "implement")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(got.Attempts))
	}
	first := got.Attempts[0].Placement
	if first == nil || first.Runner != journal.PlacementRunnerSelf || first.Host != "daemon-host" || first.OS != "linux" {
		t.Fatalf("attempt 1 placement = %+v, want self/host=daemon-host/linux", first)
	}
	if first.Node != "" {
		t.Fatalf("attempt 1 placement claims node %q — a self attempt's hostname is not a cluster node", first.Node)
	}
	if first.Pod != "" || first.QueuedAt != nil || first.PodStartedAt != nil {
		t.Fatalf("attempt 1 (self) placement carries pod/queue fields it cannot know: %+v", first)
	}
	second := got.Attempts[1].Placement
	if second == nil || second.Runner != "linux-large" || second.Pod != "goobers-stage-implement-4x2vq" ||
		second.Image != "ghcr.io/goobers/goobers-base:v0.2.0" || second.Node != "aks-linux-0001" ||
		second.Host != "goobers-stage-implement-4x2vq" {
		t.Fatalf("attempt 2 placement = %+v", second)
	}
	if second.QueuedAt == nil || !second.QueuedAt.Equal(queuedAt) ||
		second.PodStartedAt == nil || !second.PodStartedAt.Equal(podStartedAt) {
		t.Fatalf("attempt 2 dispatch timestamps = %v/%v, want %v/%v",
			second.QueuedAt, second.PodStartedAt, queuedAt, podStartedAt)
	}
}

// TestStageAttemptsWithoutPlacementReadExactlyAsToday is the zero-declaration
// invariance half (architecture §11 item 1): every pre-upgrade journal — no
// runner.placement events anywhere — must project attempts with a nil
// Placement that the wire contract omits entirely.
func TestStageAttemptsWithoutPlacementReadExactlyAsToday(t *testing.T) {
	service, layout, machine := fixtureService(t)
	run, clock := createFixtureRun(
		t, layout, machine, "run-no-placement", machine.Def.Name, "goobers",
		time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC),
		journal.Trigger{Kind: journal.TriggerManual}, false,
	)
	emit := func(event journal.Event) {
		t.Helper()
		clock.advance(time.Second)
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	emit(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1})
	emit(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 1,
		Status: string(apiv1.ResultSuccess),
	})
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := service.StageAttempts(context.Background(), "run-no-placement", "implement")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(got.Attempts))
	}
	if got.Attempts[0].Placement != nil {
		t.Fatalf("placement = %+v, want nil for a journal with no provenance", got.Attempts[0].Placement)
	}
	wire, err := json.Marshal(got.Attempts[0])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wire, []byte(`"placement"`)) {
		t.Fatalf("wire shape gained a placement key for a provenance-free attempt: %s", wire)
	}
}
