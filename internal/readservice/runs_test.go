package readservice

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

type fixtureClock struct {
	now time.Time
}

func (c *fixtureClock) advance(d time.Duration) {
	c.now = c.now.Add(d)
}

func fixtureMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	machine, err := workflow.Compile(workflow.Definition{
		Name:    "implementation",
		Version: 3,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "goobers",
			Start:  "implement",
			Tasks: []apiv1.Task{{
				Name: "implement",
				Type: apiv1.TaskAgentic,
				Goal: "implement the issue",
				Next: "review",
			}},
			Gates: []apiv1.Gate{{
				Name:      "review",
				Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "status-equals"},
				Branches: map[string]string{
					"pass": workflow.TerminalComplete,
					"fail": workflow.TargetEscalate,
				},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return machine
}

func fixtureService(t *testing.T) (*Local, instance.Layout, *workflow.Machine) {
	t.Helper()
	layout := instance.NewLayout(t.TempDir())
	service, err := NewLocal(LocalSources{
		Layout:      layout,
		Definitions: testDefinitions(),
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	return service, layout, fixtureMachine(t)
}

func createFixtureRun(
	t *testing.T,
	layout instance.Layout,
	machine *workflow.Machine,
	runID, workflowName, gaggle string,
	startedAt time.Time,
	trigger journal.Trigger,
	withGraph bool,
) (*journal.Run, *fixtureClock) {
	t.Helper()
	inputs := map[string][]byte{}
	if withGraph {
		graph, err := json.Marshal(machine.Graph())
		if err != nil {
			t.Fatal(err)
		}
		inputs[journal.PinnedWorkflowGraphInputName] = graph
	}
	clock := &fixtureClock{now: startedAt}
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID:           runID,
		Workflow:        workflowName,
		WorkflowVersion: machine.Def.Version,
		WorkflowDigest:  machine.Digest(),
		Gaggle:          gaggle,
		Trigger:         trigger,
		StartedAt:       startedAt,
	}, inputs, journal.WithClock(func() time.Time { return clock.now }))
	if err != nil {
		t.Fatal(err)
	}
	return run, clock
}

func finishFixtureRun(t *testing.T, run *journal.Run, clock *fixtureClock, phase journal.RunPhase) {
	t.Helper()
	clock.advance(time.Second)
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(phase)}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestListRunsCanonicalPhasesFiltersAndCursors(t *testing.T) {
	service, layout, machine := fixtureService(t)
	base := time.Date(2026, 7, 17, 8, 0, 0, 0, time.UTC)

	fixtures := []struct {
		id       string
		workflow string
		gaggle   string
		trigger  journal.TriggerKind
		started  time.Time
		phase    journal.RunPhase
	}{
		{id: "run-a", workflow: "implementation", gaggle: "goobers", trigger: journal.TriggerManual, started: base, phase: journal.PhaseCompleted},
		{id: "run-b", workflow: "nomination", gaggle: "goobers", trigger: journal.TriggerSchedule, started: base.Add(time.Minute), phase: journal.PhaseFailed},
		{id: "run-c", workflow: "implementation", gaggle: "other", trigger: journal.TriggerItem, started: base.Add(time.Minute), phase: journal.PhaseEscalated},
		{id: "run-d", workflow: "implementation", gaggle: "goobers", trigger: journal.TriggerManual, started: base.Add(2 * time.Minute), phase: journal.PhaseRunning},
		{id: "run-e", workflow: "implementation", gaggle: "goobers", trigger: journal.TriggerSignal, started: base.Add(3 * time.Minute), phase: journal.PhaseAborted},
	}
	for _, fixture := range fixtures {
		run, clock := createFixtureRun(
			t,
			layout,
			machine,
			fixture.id,
			fixture.workflow,
			fixture.gaggle,
			fixture.started,
			journal.Trigger{Kind: fixture.trigger},
			true,
		)
		if fixture.phase == journal.PhaseRunning {
			run.SetMachineState("implement")
			if err := run.Checkpoint(); err != nil {
				t.Fatal(err)
			}
			if err := run.Close(); err != nil {
				t.Fatal(err)
			}
			continue
		}
		finishFixtureRun(t, run, clock, fixture.phase)
	}
	if err := os.MkdirAll(filepath.Join(layout.RunsDir(), "partial-run"), 0o755); err != nil {
		t.Fatal(err)
	}

	first, err := service.ListRuns(context.Background(), RunListOptions{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Runs) != 2 || first.Runs[0].ID != "run-e" || first.Runs[1].ID != "run-d" || first.NextCursor == "" {
		t.Fatalf("first page = %+v", first)
	}
	second, err := service.ListRuns(context.Background(), RunListOptions{Limit: 2, Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Runs) != 2 || second.Runs[0].ID != "run-b" || second.Runs[1].ID != "run-c" || second.NextCursor == "" {
		t.Fatalf("second page = %+v", second)
	}
	third, err := service.ListRuns(context.Background(), RunListOptions{Limit: 2, Cursor: second.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Runs) != 1 || third.Runs[0].ID != "run-a" || third.NextCursor != "" {
		t.Fatalf("third page = %+v", third)
	}

	filtered, err := service.ListRuns(context.Background(), RunListOptions{
		Workflow: "implementation",
		Gaggle:   "goobers",
		Trigger:  journal.TriggerManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Runs) != 2 || filtered.Runs[0].ID != "run-d" || filtered.Runs[1].ID != "run-a" {
		t.Fatalf("filtered runs = %+v", filtered.Runs)
	}
	completed, err := service.ListRuns(context.Background(), RunListOptions{Phase: journal.PhaseCompleted})
	if err != nil {
		t.Fatal(err)
	}
	if len(completed.Runs) != 1 || completed.Runs[0].ID != "run-a" {
		t.Fatalf("completed runs = %+v", completed.Runs)
	}

	if _, err := service.ListRuns(context.Background(), RunListOptions{Cursor: "not-base64"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("malformed cursor error = %v", err)
	}
	if _, err := service.ListRuns(context.Background(), RunListOptions{Limit: maxRunLimit + 1}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("oversized limit error = %v", err)
	}
	if _, err := service.ListRuns(context.Background(), RunListOptions{Phase: "succeeded"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("non-canonical phase error = %v", err)
	}
	if _, err := service.ListRuns(context.Background(), RunListOptions{Trigger: "webhook"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("non-canonical trigger error = %v", err)
	}
	if _, err := service.GetRun(context.Background(), "partial-run"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partial run error = %v", err)
	}
}

func TestRunDetailEventsAttemptsAndPinnedGraph(t *testing.T) {
	service, layout, machine := fixtureService(t)
	started := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	run, clock := createFixtureRun(
		t,
		layout,
		machine,
		"run-detail",
		machine.Def.Name,
		"goobers",
		started,
		journal.Trigger{Kind: journal.TriggerItem, Ref: "511"},
		true,
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
		Type:    journal.EventStageFinished,
		Stage:   "implement",
		Attempt: 1,
		Status:  string(apiv1.ResultFailure),
	})
	appendEvent(journal.Event{
		Type:         journal.EventStageStarted,
		Branch:       7,
		Stage:        "implement",
		Attempt:      2,
		AttemptClass: journal.AttemptPolicy,
	})
	clock.advance(time.Second)
	artifactRef, err := run.RecordStageArtifact(
		"implement",
		2,
		journal.AttemptPolicy,
		"result.json",
		[]byte(`{"status":"fixed"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	artifactRef.MediaType = "application/json"
	appendEvent(journal.Event{
		Type:         journal.EventStageFinished,
		Stage:        "implement",
		Attempt:      2,
		AttemptClass: journal.AttemptPolicy,
		Status:       string(apiv1.ResultSuccess),
		Outputs: map[string]any{
			"changed": true,
			"count":   2,
			"nested":  map[string]any{"not": "scalar"},
		},
		Artifacts: []journal.Ref{artifactRef},
	})
	appendEvent(journal.Event{
		Type:    journal.EventGateEvaluated,
		Gate:    "review",
		Verdict: "needs-changes",
		Target:  "implement",
	})
	appendEvent(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1})
	appendEvent(journal.Event{
		Type:    journal.EventStageFinished,
		Stage:   "implement",
		Attempt: 1,
		Status:  string(apiv1.ResultFailure),
	})
	appendEvent(journal.Event{
		Type:         journal.EventStageStarted,
		Stage:        "implement",
		Attempt:      2,
		AttemptClass: journal.AttemptInfra,
	})
	appendEvent(journal.Event{
		Type:         journal.EventStageFinished,
		Stage:        "implement",
		Attempt:      2,
		AttemptClass: journal.AttemptInfra,
		Status:       string(apiv1.ResultSuccess),
	})
	appendEvent(journal.Event{
		Type:    journal.EventGateEvaluated,
		Gate:    "review",
		Verdict: "fail",
		Target:  workflow.TargetEscalate,
		Runner: map[string]any{
			"repassAttempt": 4,
			"escalated":     true,
			"duplicateDiff": false,
		},
	})
	appendEvent(journal.Event{
		Type:   journal.EventRunFinished,
		Status: string(journal.PhaseEscalated),
	})
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	detail, err := service.GetRun(context.Background(), "run-detail")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Phase != journal.PhaseEscalated || !detail.Terminal || detail.CurrentStage != "" {
		t.Fatalf("run state = %+v", detail.RunSummary)
	}
	if detail.RepassCount != 1 || detail.RetryCount != 2 ||
		detail.PolicyRetryCount != 1 || detail.InfraRetryCount != 1 {
		t.Fatalf("attempt counts = %+v", detail.RunSummary)
	}
	if detail.GraphStatus != "pinned" || detail.Graph == nil ||
		detail.Graph.Digest != machine.Digest() || detail.Graph.Start != "implement" {
		t.Fatalf("pinned graph = %#v (%s)", detail.Graph, detail.GraphStatus)
	}
	if detail.Escalation == nil ||
		detail.Escalation.Selector.Name != "review" ||
		detail.Escalation.SelectedBranch != "fail" ||
		detail.Escalation.RepassCount != 4 ||
		detail.Escalation.TerminalReason != "repass budget exhausted" {
		t.Fatalf("escalation = %+v", detail.Escalation)
	}
	traceEscalation, err := service.RunEscalation(context.Background(), "run-detail")
	if err != nil {
		t.Fatal(err)
	}
	if traceEscalation == nil || traceEscalation.RepassCount != 4 {
		t.Fatalf("trace escalation = %+v", traceEscalation)
	}

	events, err := service.RunEvents(context.Background(), "run-detail")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(events.Events); i++ {
		if events.Events[i-1].Seq > events.Events[i].Seq {
			t.Fatalf("events not in sequence order: %+v", events.Events)
		}
	}
	var policyStart *RunEvent
	var escalationSeq uint64
	for i := range events.Events {
		event := &events.Events[i]
		if policyStart == nil && event.Type == journal.EventStageStarted && event.AttemptClass == "policy" {
			policyStart = event
		}
		if event.Type == journal.EventGateEvaluated && event.Target == workflow.TargetEscalate {
			escalationSeq = event.Seq
		}
	}
	if policyStart == nil || policyStart.Attempt != 2 || policyStart.Branch != 7 {
		t.Fatalf("policy event = %+v", policyStart)
	}
	if detail.Escalation.CausalEventSeq != escalationSeq {
		t.Fatalf("escalation causal event = %d, want %d", detail.Escalation.CausalEventSeq, escalationSeq)
	}

	attempts, err := service.StageAttempts(context.Background(), "run-detail", "implement")
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts.Attempts) != 4 {
		t.Fatalf("attempts = %+v", attempts.Attempts)
	}
	classes := []string{
		attempts.Attempts[0].Class,
		attempts.Attempts[1].Class,
		attempts.Attempts[2].Class,
		attempts.Attempts[3].Class,
	}
	if strings.Join(classes, ",") != "initial,policy,initial,infra" {
		t.Fatalf("attempt classes = %v", classes)
	}
	policy := attempts.Attempts[1]
	if policy.Outputs["changed"] != true || policy.Outputs["count"] != float64(2) {
		t.Fatalf("scalar outputs = %#v", policy.Outputs)
	}
	if _, ok := policy.Outputs["nested"]; ok {
		t.Fatalf("nested output should not be exposed as scalar: %#v", policy.Outputs)
	}
	if len(policy.Artifacts) != 1 ||
		policy.Artifacts[0].Digest != artifactRef.Digest ||
		policy.Artifacts[0].MediaType != "application/json" {
		t.Fatalf("attempt artifacts = %+v", policy.Artifacts)
	}
}

func TestStageRerunRequestReturnsEscalatedRunToRunning(t *testing.T) {
	service, layout, machine := fixtureService(t)
	started := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)
	run, clock := createFixtureRun(
		t,
		layout,
		machine,
		"run-rerun-requested",
		machine.Def.Name,
		"goobers",
		started,
		journal.Trigger{Kind: journal.TriggerManual},
		false,
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
		Status: string(apiv1.ResultBlocked),
	})
	appendEvent(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)})
	appendEvent(journal.Event{
		Type: journal.EventStageRerunRequested, Stage: "implement", Attempt: 2,
		AttemptClass: journal.AttemptHuman, Actor: "maintainer",
		InstructionAddendum: "Reuse the parser seam.",
	})
	appendEvent(journal.Event{
		Type: journal.EventStageStarted, Stage: "implement", Attempt: 2,
		AttemptClass: journal.AttemptHuman,
	})
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	detail, err := service.GetRun(context.Background(), "run-rerun-requested")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Phase != journal.PhaseRunning || detail.Terminal || detail.CurrentStage != "implement" {
		t.Fatalf("rerun detail = %+v", detail.RunSummary)
	}
	if detail.RepassCount != 0 || detail.RetryCount != 0 {
		t.Fatalf("human rerun changed automatic attempt counts: %+v", detail.RunSummary)
	}
}

func TestLiveRunDurationUsesObservationTime(t *testing.T) {
	service, layout, machine := fixtureService(t)
	started := time.Date(2026, 7, 17, 9, 15, 0, 0, time.UTC)
	run, clock := createFixtureRun(
		t,
		layout,
		machine,
		"run-live-duration",
		machine.Def.Name,
		"goobers",
		started,
		journal.Trigger{Kind: journal.TriggerManual},
		false,
	)
	clock.advance(2 * time.Second)
	if err := run.Append(journal.Event{Type: journal.EventGateStarted, Gate: "review"}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return started.Add(30 * time.Second) }

	detail, err := service.GetRun(context.Background(), "run-live-duration")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Terminal || detail.DurationMillis != 30_000 {
		t.Fatalf("live run summary = %+v", detail.RunSummary)
	}
	if want := started.Add(2 * time.Second); !detail.LastActivityAt.Equal(want) {
		t.Fatalf("last activity = %s, want %s", detail.LastActivityAt, want)
	}
}

func TestUnknownSchemasAndTornTailRemainInspectable(t *testing.T) {
	service, layout, machine := fixtureService(t)
	run, _ := createFixtureRun(
		t,
		layout,
		machine,
		"run-future",
		machine.Def.Name,
		"goobers",
		time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC),
		journal.Trigger{Kind: journal.TriggerManual},
		false,
	)
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	eventsPath := filepath.Join(layout.RunsDir(), "run-future", "events.jsonl")
	file, err := os.OpenFile(eventsPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	unknown := `{"schema":"goobers.dev/journal/event/v99","seq":2,"type":"future.event","branch":4,"time":"2026-07-17T10:00:01Z","status":"completed","future":{"answer":42}}`
	if _, err := file.WriteString(unknown + "\n" + `{"schema":"goobers.dev/journal/event/v99"`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	detail, err := service.GetRun(context.Background(), "run-future")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Phase != journal.PhaseRunning || detail.Graph != nil || detail.GraphStatus != "unavailable" {
		t.Fatalf("future run detail = %+v", detail)
	}
	events, err := service.RunEvents(context.Background(), "run-future")
	if err != nil {
		t.Fatal(err)
	}
	if len(events.Events) != 2 {
		t.Fatalf("events = %+v", events.Events)
	}
	future := events.Events[1]
	if future.KnownSchema || future.Seq != 2 || future.Branch != 4 ||
		!strings.Contains(string(future.Raw), `"answer":42`) {
		t.Fatalf("future event = %+v", future)
	}
	if future.Status != "" {
		t.Fatalf("unsupported type-specific status was trusted: %+v", future)
	}
}

func TestArtifactReadsAreRedactedContainedTypedAndVerified(t *testing.T) {
	service, layout, machine := fixtureService(t)
	run, clock := createFixtureRun(
		t,
		layout,
		machine,
		"run-artifact",
		machine.Def.Name,
		"goobers",
		time.Date(2026, 7, 17, 11, 0, 0, 0, time.UTC),
		journal.Trigger{Kind: journal.TriggerManual},
		true,
	)
	clock.advance(time.Second)
	token := "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789AB"
	ref, err := run.RecordStageArtifact(
		"implement",
		1,
		"",
		"diagnostic.json",
		[]byte(`{"token":"`+token+`"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	ref.MediaType = "application/json"
	clock.advance(time.Second)
	if err := run.Append(journal.Event{
		Type:      journal.EventStageFinished,
		Stage:     "implement",
		Attempt:   1,
		Status:    string(apiv1.ResultSuccess),
		Artifacts: []journal.Ref{ref},
	}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	artifact, err := service.Artifact(context.Background(), "run-artifact", ref.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Metadata.MediaType != "application/json" ||
		artifact.Metadata.Name != "diagnostic.json" ||
		strings.Contains(string(artifact.Bytes), token) ||
		!strings.Contains(string(artifact.Bytes), "[REDACTED]") {
		t.Fatalf("artifact = metadata %+v, bytes %q", artifact.Metadata, artifact.Bytes)
	}
	if _, err := service.Artifact(context.Background(), "../escape", ref.Digest); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("run traversal error = %v", err)
	}
	if _, err := service.Artifact(context.Background(), "run-artifact", "../../etc/passwd"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("digest traversal error = %v", err)
	}

	if err := os.WriteFile(filepath.Join(layout.RunsDir(), "run-artifact", ref.Path), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Artifact(context.Background(), "run-artifact", ref.Digest); !errors.Is(err, ErrArtifactIntegrity) {
		t.Fatalf("tampered artifact error = %v", err)
	}

	escapeRun, escapeClock := createFixtureRun(
		t,
		layout,
		machine,
		"run-escape",
		machine.Def.Name,
		"goobers",
		time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC),
		journal.Trigger{Kind: journal.TriggerManual},
		true,
	)
	escapeClock.advance(time.Second)
	outsideDigest := journal.Digest([]byte("outside"))
	if err := escapeRun.Append(journal.Event{
		Type: journal.EventArtifactRecorded,
		Name: "escape",
		Ref: &journal.Ref{
			Path:   "../../outside",
			Digest: outsideDigest,
			Size:   7,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := escapeRun.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Artifact(context.Background(), "run-escape", outsideDigest); !errors.Is(err, ErrNotFound) {
		t.Fatalf("uncontained artifact error = %v", err)
	}
}

func TestPinnedGraphTamperFailsClosed(t *testing.T) {
	service, layout, machine := fixtureService(t)
	run, _ := createFixtureRun(
		t,
		layout,
		machine,
		"run-graph-tamper",
		machine.Def.Name,
		"goobers",
		time.Date(2026, 7, 17, 13, 0, 0, 0, time.UTC),
		journal.Trigger{Kind: journal.TriggerManual},
		true,
	)
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenRead(filepath.Join(layout.RunsDir(), "run-graph-tamper"))
	if err != nil {
		t.Fatal(err)
	}
	identity, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(reader.Dir(), identity.Inputs[0].Ref.Path),
		[]byte(`{"name":"mutable-current-config"}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetRun(context.Background(), "run-graph-tamper"); !errors.Is(err, ErrArtifactIntegrity) {
		t.Fatalf("tampered graph error = %v", err)
	}
}

func TestExecutorArtifactsInheritStageAttemptMetadata(t *testing.T) {
	service, layout, machine := fixtureService(t)
	run, clock := createFixtureRun(
		t,
		layout,
		machine,
		"run-executor-artifacts",
		machine.Def.Name,
		"goobers",
		time.Date(2026, 7, 17, 13, 30, 0, 0, time.UTC),
		journal.Trigger{Kind: journal.TriggerManual},
		false,
	)
	clock.advance(time.Second)
	if err := run.Append(journal.Event{
		Type:    journal.EventStageStarted,
		Stage:   "implement",
		Attempt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Second)
	stdout, err := run.RecordArtifact("implement/stdout.log", []byte("done"))
	if err != nil {
		t.Fatal(err)
	}
	stdout.MediaType = "text/plain"
	clock.advance(time.Second)
	if err := run.Append(journal.Event{
		Type:      journal.EventStageFinished,
		Stage:     "implement",
		Attempt:   1,
		Status:    string(apiv1.ResultSuccess),
		Artifacts: []journal.Ref{stdout},
	}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	attempts, err := service.StageAttempts(context.Background(), "run-executor-artifacts", "implement")
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts.Attempts) != 1 || len(attempts.Attempts[0].Artifacts) != 1 {
		t.Fatalf("attempt artifacts = %+v", attempts.Attempts)
	}
	artifact := attempts.Attempts[0].Artifacts[0]
	if artifact.Name != "implement/stdout.log" ||
		artifact.Stage != "implement" ||
		artifact.Attempt != 1 ||
		artifact.AttemptClass != "initial" ||
		artifact.MediaType != "text/plain" {
		t.Fatalf("artifact metadata = %+v", artifact)
	}

	events, err := service.RunEvents(context.Background(), "run-executor-artifacts")
	if err != nil {
		t.Fatal(err)
	}
	finished := events.Events[len(events.Events)-1]
	if finished.Type != journal.EventStageFinished ||
		len(finished.Artifacts) != 1 ||
		finished.Artifacts[0].RecordedSeq != artifact.RecordedSeq {
		t.Fatalf("stage finished artifacts = %+v", finished)
	}
}

func TestAttemptsCloseDispatchErrorsAndKeepRepassArtifactsSeparate(t *testing.T) {
	service, layout, machine := fixtureService(t)
	run, clock := createFixtureRun(
		t,
		layout,
		machine,
		"run-attempt-edges",
		machine.Def.Name,
		"goobers",
		time.Date(2026, 7, 17, 14, 0, 0, 0, time.UTC),
		journal.Trigger{Kind: journal.TriggerManual},
		false,
	)

	appendEvent := func(event journal.Event) {
		t.Helper()
		clock.advance(time.Second)
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	recordTraversal := func(mediaType string) journal.Ref {
		t.Helper()
		appendEvent(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1})
		clock.advance(time.Second)
		ref, err := run.RecordStageArtifact("implement", 1, "", "same.json", []byte(`{"same":true}`))
		if err != nil {
			t.Fatal(err)
		}
		ref.MediaType = mediaType
		appendEvent(journal.Event{
			Type:      journal.EventStageFinished,
			Stage:     "implement",
			Attempt:   1,
			Status:    string(apiv1.ResultSuccess),
			Artifacts: []journal.Ref{ref},
		})
		return ref
	}

	first := recordTraversal("text/plain")
	second := recordTraversal("application/json")
	if first.Digest != second.Digest {
		t.Fatal("test requires duplicate artifact content across repasses")
	}
	appendEvent(journal.Event{
		Type:         journal.EventStageStarted,
		Stage:        "implement",
		Attempt:      2,
		AttemptClass: journal.AttemptInfra,
	})
	appendEvent(journal.Event{
		Type:         journal.EventError,
		Stage:        "implement",
		Attempt:      2,
		AttemptClass: journal.AttemptInfra,
		Error:        &journal.ErrorDetail{Code: "executor_error", Message: "worker disappeared"},
	})
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := service.StageAttempts(context.Background(), "run-attempt-edges", "implement")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Attempts) != 3 {
		t.Fatalf("attempts = %+v", got.Attempts)
	}
	if len(got.Attempts[0].Artifacts) != 1 ||
		got.Attempts[0].Artifacts[0].MediaType != "text/plain" ||
		len(got.Attempts[1].Artifacts) != 1 ||
		got.Attempts[1].Artifacts[0].MediaType != "application/json" ||
		got.Attempts[0].Artifacts[0].RecordedSeq == got.Attempts[1].Artifacts[0].RecordedSeq {
		t.Fatalf("repass artifacts = %+v", got.Attempts)
	}
	failed := got.Attempts[2]
	if failed.Status != string(apiv1.ResultFailure) ||
		failed.FinishedSeq == 0 ||
		failed.Error == nil ||
		failed.Error.Code != "executor_error" {
		t.Fatalf("dispatch failure attempt = %+v", failed)
	}
}

func TestArtifactRedactionReplacesDigestAndPreservesMetadata(t *testing.T) {
	service, layout, machine := fixtureService(t)
	registry, scrubber := journal.DefaultScrubber()
	clock := &fixtureClock{now: time.Date(2026, 7, 17, 15, 0, 0, 0, time.UTC)}
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID:           "run-redacted-artifact",
		Workflow:        machine.Def.Name,
		WorkflowVersion: machine.Def.Version,
		WorkflowDigest:  machine.Digest(),
		Gaggle:          "goobers",
		Trigger:         journal.Trigger{Kind: journal.TriggerManual},
		StartedAt:       clock.now,
	}, nil, journal.WithScrubber(scrubber), journal.WithClock(func() time.Time { return clock.now }))
	if err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Second)
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	const leaked = "PLAINTEXT-LEAK-api-artifact"
	clock.advance(time.Second)
	oldRef, err := run.RecordArtifact("diagnostic.json", []byte(`{"value":"`+leaked+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	typedRef := oldRef
	typedRef.MediaType = "application/json"
	clock.advance(time.Second)
	if err := run.Append(journal.Event{
		Type:      journal.EventStageFinished,
		Stage:     "implement",
		Attempt:   1,
		Status:    string(apiv1.ResultSuccess),
		Artifacts: []journal.Ref{typedRef},
	}); err != nil {
		t.Fatal(err)
	}
	registry.Register([]byte(leaked))
	clock.advance(time.Second)
	newRef, err := run.Redact(oldRef, "credential remediation")
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := service.Artifact(context.Background(), "run-redacted-artifact", oldRef.Digest); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old digest error = %v", err)
	}
	artifact, err := service.Artifact(context.Background(), "run-redacted-artifact", newRef.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Metadata.Name != "diagnostic.json" ||
		artifact.Metadata.MediaType != "application/json" ||
		strings.Contains(string(artifact.Bytes), leaked) ||
		!strings.Contains(string(artifact.Bytes), "[REDACTED]") {
		t.Fatalf("redacted artifact = metadata %+v, bytes %q", artifact.Metadata, artifact.Bytes)
	}
	attempts, err := service.StageAttempts(context.Background(), "run-redacted-artifact", "implement")
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts.Attempts) != 1 ||
		len(attempts.Attempts[0].Artifacts) != 1 ||
		attempts.Attempts[0].Artifacts[0].Digest != newRef.Digest {
		t.Fatalf("redacted attempt artifacts = %+v", attempts.Attempts)
	}
	events, err := service.RunEvents(context.Background(), "run-redacted-artifact")
	if err != nil {
		t.Fatal(err)
	}
	var finishedArtifacts []ArtifactMetadata
	for _, event := range events.Events {
		if event.Type == journal.EventStageFinished {
			finishedArtifacts = event.Artifacts
			break
		}
	}
	if len(finishedArtifacts) != 1 || finishedArtifacts[0].Digest != newRef.Digest {
		t.Fatalf("redacted stage event artifacts = %+v", finishedArtifacts)
	}
}

func TestDirectStageEscalationIncludesCause(t *testing.T) {
	service, layout, machine := fixtureService(t)
	run, clock := createFixtureRun(
		t,
		layout,
		machine,
		"run-direct-escalation",
		machine.Def.Name,
		"goobers",
		time.Date(2026, 7, 17, 16, 0, 0, 0, time.UTC),
		journal.Trigger{Kind: journal.TriggerItem, Ref: "511"},
		false,
	)
	clock.advance(time.Second)
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Second)
	if err := run.Append(journal.Event{
		Type:  journal.EventError,
		Stage: "implement",
		Error: &journal.ErrorDetail{Code: "worktree_remove_failed", Message: "cleanup failed"},
	}); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Second)
	if err := run.Append(journal.Event{
		Type:    journal.EventStageFinished,
		Stage:   "implement",
		Attempt: 1,
		Status:  string(apiv1.ResultFailure),
		Error:   &journal.ErrorDetail{Code: "ISSUE_OVER_SCOPE", Message: "split this work"},
	}); err != nil {
		t.Fatal(err)
	}
	finishFixtureRun(t, run, clock, journal.PhaseEscalated)

	detail, err := service.GetRun(context.Background(), "run-direct-escalation")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Escalation == nil ||
		detail.Escalation.Selector.Kind != "stage" ||
		detail.Escalation.Selector.Name != "implement" ||
		detail.Escalation.TerminalReason != "split this work" {
		t.Fatalf("direct escalation = %+v", detail.Escalation)
	}
	traceEscalation, err := service.RunEscalation(context.Background(), "run-direct-escalation")
	if err != nil {
		t.Fatal(err)
	}
	if traceEscalation == nil || traceEscalation.RepassCount != 0 {
		t.Fatalf("trace escalation = %+v", traceEscalation)
	}
	events, err := service.RunEvents(context.Background(), "run-direct-escalation")
	if err != nil {
		t.Fatal(err)
	}
	var stageSeq, cleanupSeq uint64
	for _, event := range events.Events {
		switch {
		case event.Type == journal.EventStageFinished:
			stageSeq = event.Seq
		case event.Type == journal.EventError && event.Error != nil && event.Error.Code == "worktree_remove_failed":
			cleanupSeq = event.Seq
		}
	}
	if detail.Escalation.CausalEventSeq != stageSeq || detail.Escalation.CausalEventSeq == cleanupSeq {
		t.Fatalf("causal event = %d, stage = %d, cleanup = %d", detail.Escalation.CausalEventSeq, stageSeq, cleanupSeq)
	}
}

func TestParkedStageEscalationUsesOriginatingFailure(t *testing.T) {
	service, layout, machine := fixtureService(t)
	run, clock := createFixtureRun(
		t,
		layout,
		machine,
		"run-parked-stage-escalation",
		machine.Def.Name,
		"goobers",
		time.Date(2026, 7, 17, 16, 15, 0, 0, time.UTC),
		journal.Trigger{Kind: journal.TriggerItem, Ref: "511"},
		false,
	)
	clock.advance(time.Second)
	if err := run.Append(journal.Event{
		Type:    journal.EventStageStarted,
		Stage:   "implement",
		Attempt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Second)
	if err := run.Append(journal.Event{
		Type:    journal.EventStageFinished,
		Stage:   "implement",
		Attempt: 1,
		Status:  string(apiv1.ResultFailure),
		Error:   &journal.ErrorDetail{Code: "NEEDS_DECOMPOSITION", Message: "split this issue"},
	}); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Second)
	if err := run.Append(journal.Event{
		Type:    journal.EventStageStarted,
		Stage:   "park-escalated",
		Attempt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Second)
	if err := run.Append(journal.Event{
		Type:    journal.EventStageFinished,
		Stage:   "park-escalated",
		Attempt: 1,
		Status:  string(apiv1.ResultSuccess),
	}); err != nil {
		t.Fatal(err)
	}
	finishFixtureRun(t, run, clock, journal.PhaseEscalated)

	detail, err := service.GetRun(context.Background(), "run-parked-stage-escalation")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Escalation == nil ||
		detail.Escalation.Selector.Kind != "stage" ||
		detail.Escalation.Selector.Name != "implement" ||
		detail.Escalation.TerminalReason != "split this issue" {
		t.Fatalf("parked stage escalation = %+v", detail.Escalation)
	}
	events, err := service.RunEvents(context.Background(), "run-parked-stage-escalation")
	if err != nil {
		t.Fatal(err)
	}
	var implementSeq, parkingSeq uint64
	for _, event := range events.Events {
		if event.Type != journal.EventStageFinished {
			continue
		}
		switch event.Stage {
		case "implement":
			implementSeq = event.Seq
		case "park-escalated":
			parkingSeq = event.Seq
		}
	}
	if detail.Escalation.CausalEventSeq != implementSeq ||
		detail.Escalation.CausalEventSeq == parkingSeq {
		t.Fatalf(
			"causal event = %d, implement = %d, parking = %d",
			detail.Escalation.CausalEventSeq,
			implementSeq,
			parkingSeq,
		)
	}
}

func TestDuplicateDiffEscalationUsesRunnerMetadata(t *testing.T) {
	service, layout, machine := fixtureService(t)
	run, clock := createFixtureRun(
		t,
		layout,
		machine,
		"run-duplicate-diff",
		machine.Def.Name,
		"goobers",
		time.Date(2026, 7, 17, 16, 30, 0, 0, time.UTC),
		journal.Trigger{Kind: journal.TriggerItem, Ref: "511"},
		false,
	)
	clock.advance(time.Second)
	if err := run.Append(journal.Event{
		Type:    journal.EventGateEvaluated,
		Gate:    "review",
		Verdict: "needs-changes",
		Target:  workflow.TargetEscalate,
		Runner: map[string]any{
			"repassAttempt": 2,
			"escalated":     true,
			"duplicateDiff": true,
			"diffDigest":    "sha256:aaaa",
		},
	}); err != nil {
		t.Fatal(err)
	}
	finishFixtureRun(t, run, clock, journal.PhaseEscalated)

	detail, err := service.GetRun(context.Background(), "run-duplicate-diff")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Escalation == nil ||
		detail.Escalation.Selector.Kind != "gate" ||
		detail.Escalation.Selector.Name != "review" ||
		detail.Escalation.SelectedBranch != "needs-changes" ||
		detail.Escalation.TerminalReason != "repass produced a diff identical to the immediately prior attempt" {
		t.Fatalf("duplicate-diff escalation = %+v", detail.Escalation)
	}
}

func TestRoutedGateEscalationIncludesCause(t *testing.T) {
	service, layout, machine := fixtureService(t)
	run, clock := createFixtureRun(
		t,
		layout,
		machine,
		"run-routed-escalation",
		machine.Def.Name,
		"goobers",
		time.Date(2026, 7, 17, 16, 40, 0, 0, time.UTC),
		journal.Trigger{Kind: journal.TriggerItem, Ref: "511"},
		false,
	)
	clock.advance(time.Second)
	if err := run.Append(journal.Event{
		Type:      journal.EventGateEvaluated,
		Gate:      "review",
		Verdict:   string(apiv1.VerdictNeedsChanges),
		Target:    "park-escalated",
		Escalated: true,
		Runner: map[string]any{
			"repassAttempt": 4,
		},
	}); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Second)
	if err := run.Append(journal.Event{
		Type:    journal.EventStageFinished,
		Stage:   "park-escalated",
		Attempt: 1,
		Status:  string(apiv1.ResultSuccess),
	}); err != nil {
		t.Fatal(err)
	}
	finishFixtureRun(t, run, clock, journal.PhaseEscalated)

	detail, err := service.GetRun(context.Background(), "run-routed-escalation")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Escalation == nil ||
		detail.Escalation.Selector.Kind != "gate" ||
		detail.Escalation.Selector.Name != "review" ||
		detail.Escalation.SelectedBranch != string(apiv1.VerdictNeedsChanges) ||
		detail.Escalation.RepassCount != 4 ||
		detail.Escalation.TerminalReason != "repass budget exhausted" {
		t.Fatalf("routed escalation = %+v", detail.Escalation)
	}
	traceEscalation, err := service.RunEscalation(context.Background(), "run-routed-escalation")
	if err != nil {
		t.Fatal(err)
	}
	if traceEscalation == nil || traceEscalation.RepassCount != 4 {
		t.Fatalf("trace escalation = %+v", traceEscalation)
	}
	events, err := service.RunEvents(context.Background(), "run-routed-escalation")
	if err != nil {
		t.Fatal(err)
	}
	var projectedEscalation bool
	for _, event := range events.Events {
		if event.Type == journal.EventGateEvaluated {
			projectedEscalation = event.Escalated
		}
	}
	if !projectedEscalation {
		t.Fatal("routed gate event did not expose escalated=true")
	}
}

func TestRoutedGateEscalationMarkersIncludeCause(t *testing.T) {
	for _, tc := range []struct {
		name   string
		legacy bool
	}{
		{name: "typed event field"},
		{name: "legacy runner metadata", legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := journal.Event{
				Schema:    journal.EventSchema,
				Seq:       7,
				Type:      journal.EventGateEvaluated,
				Gate:      "review",
				Verdict:   string(apiv1.VerdictNeedsChanges),
				Target:    "park-escalated",
				Escalated: !tc.legacy,
				Runner:    map[string]any{"repassAttempt": 4},
			}
			if tc.legacy {
				event.Runner["escalated"] = true
			}
			cause, err := escalationCause(
				RunSummary{Phase: journal.PhaseEscalated},
				[]journal.EventRecord{{Event: event}},
			)
			if err != nil {
				t.Fatal(err)
			}
			if cause == nil ||
				cause.Selector != (EscalationSelector{Kind: "gate", Name: "review"}) ||
				cause.SelectedBranch != string(apiv1.VerdictNeedsChanges) ||
				cause.RepassCount != 4 ||
				cause.TerminalReason != "repass budget exhausted" ||
				cause.CausalEventSeq != 7 {
				t.Fatalf("escalation cause = %+v", cause)
			}
		})
	}
}

func TestRoutedGateFailureIncludesCause(t *testing.T) {
	service, layout, machine := fixtureService(t)
	run, clock := createFixtureRun(
		t,
		layout,
		machine,
		"run-routed-failure",
		machine.Def.Name,
		"goobers",
		time.Date(2026, 7, 17, 16, 42, 0, 0, time.UTC),
		journal.Trigger{Kind: journal.TriggerItem, Ref: "511"},
		false,
	)
	clock.advance(time.Second)
	if err := run.Append(journal.Event{
		Type:    journal.EventGateEvaluated,
		Gate:    "review",
		Verdict: string(apiv1.VerdictFail),
		Target:  "park-escalated",
	}); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Second)
	if err := run.Append(journal.Event{
		Type:    journal.EventStageFinished,
		Stage:   "park-escalated",
		Attempt: 1,
		Status:  string(apiv1.ResultSuccess),
	}); err != nil {
		t.Fatal(err)
	}
	finishFixtureRun(t, run, clock, journal.PhaseEscalated)

	detail, err := service.GetRun(context.Background(), "run-routed-failure")
	if err != nil {
		t.Fatal(err)
	}
	events, err := service.RunEvents(context.Background(), "run-routed-failure")
	if err != nil {
		t.Fatal(err)
	}
	var gateSeq uint64
	for _, event := range events.Events {
		if event.Type == journal.EventGateEvaluated {
			gateSeq = event.Seq
			break
		}
	}
	if detail.Escalation == nil ||
		detail.Escalation.Selector.Kind != "gate" ||
		detail.Escalation.Selector.Name != "review" ||
		detail.Escalation.SelectedBranch != string(apiv1.VerdictFail) ||
		detail.Escalation.RepassCount != 1 ||
		detail.Escalation.TerminalReason != "gate review resolved fail -> park-escalated" ||
		gateSeq == 0 ||
		detail.Escalation.CausalEventSeq != gateSeq {
		t.Fatalf("routed gate failure = %+v", detail.Escalation)
	}
}

func TestBlockedStageEscalationUsesRecordedReason(t *testing.T) {
	service, layout, machine := fixtureService(t)
	run, clock := createFixtureRun(
		t,
		layout,
		machine,
		"run-blocked",
		machine.Def.Name,
		"goobers",
		time.Date(2026, 7, 17, 16, 45, 0, 0, time.UTC),
		journal.Trigger{Kind: journal.TriggerItem, Ref: "511"},
		false,
	)
	clock.advance(time.Second)
	if err := run.Append(journal.Event{
		Type:    journal.EventStageFinished,
		Stage:   "implement",
		Attempt: 1,
		Status:  string(apiv1.ResultBlocked),
	}); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Second)
	if err := run.Append(journal.Event{
		Type:  journal.EventError,
		Stage: "implement",
		Error: &journal.ErrorDetail{Code: "blocked_by_agent", Message: "waiting for issue 441"},
	}); err != nil {
		t.Fatal(err)
	}
	finishFixtureRun(t, run, clock, journal.PhaseEscalated)

	detail, err := service.GetRun(context.Background(), "run-blocked")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Escalation == nil ||
		detail.Escalation.Selector.Kind != "stage" ||
		detail.Escalation.Selector.Name != "implement" ||
		detail.Escalation.TerminalReason != "waiting for issue 441" {
		t.Fatalf("blocked escalation = %+v", detail.Escalation)
	}
}

func TestArtifactReadRejectsSymlinkEscape(t *testing.T) {
	service, layout, machine := fixtureService(t)
	run, _ := createFixtureRun(
		t,
		layout,
		machine,
		"run-artifact-symlink",
		machine.Def.Name,
		"goobers",
		time.Date(2026, 7, 17, 17, 0, 0, 0, time.UTC),
		journal.Trigger{Kind: journal.TriggerManual},
		false,
	)
	ref, err := run.RecordArtifact("outside.txt", []byte("outside"))
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(layout.RunsDir(), "run-artifact-symlink", ref.Path)
	if err := os.Remove(artifactPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, artifactPath); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Artifact(context.Background(), "run-artifact-symlink", ref.Digest); !errors.Is(err, ErrArtifactIntegrity) {
		t.Fatalf("symlink artifact error = %v", err)
	}
}
