package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/handoffcheck"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

const runnerHandoffSchema = `{
  "type": "object",
  "required": ["verdict"],
  "additionalProperties": false,
  "properties": {
    "verdict": {"enum": ["pass", "fail"]}
  }
}`

func publishHandoffSet(t *testing.T, root, payload string) []apiv1.ContextPointer {
	t.Helper()
	record := func(name, mediaType string, data []byte) (apiv1.ArtifactPointer, error) {
		return apiv1.WriteArtifact(root, filepath.Join("artifacts", "produce", name), data, mediaType)
	}
	prepared, err := artifactset.Prepare(context.Background(), writeManifestWorkspace(t, payload), "manifest.json", func(_ string, data []byte) ([]byte, error) {
		return data, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.Bind(&apiv1.ArtifactPublication{
		Stage: "produce", Visit: 1, Slots: []apiv1.ArtifactSlot{{Name: "report", MediaType: "application/json"}},
	}, 1); err != nil {
		t.Fatal(err)
	}
	pointers, err := prepared.Publish(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	return contextPointersFor("produce", pointers)
}

func writeManifestWorkspace(t *testing.T, payload string) string {
	t.Helper()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "payload.json"), []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "manifest.json"), []byte(`{"schemaVersion":"goobers.dev/stage-artifact-set/v1alpha1","entries":[{"name":"report","path":"payload.json","mediaType":"application/json"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return workspace
}

func testHandoffSchemaLoader(t *testing.T) HandoffSchemaLoader {
	t.Helper()
	return func(schemaPath string) (*handoffcheck.Schema, error) {
		return handoffcheck.Compile(schemaPath, "", []byte(runnerHandoffSchema))
	}
}

func TestBuildHandoffValidationReportValid(t *testing.T) {
	root := t.TempDir()
	report := buildHandoffValidationReport(context.Background(), root, map[string]handoffBinding{
		"summary": {LocalName: "summary", ProducerTask: "produce", SlotName: "report", MediaType: "application/json", SchemaPath: "schemas/report.schema.json"},
	}, publishHandoffSet(t, root, `{"verdict":"pass"}`), testHandoffSchemaLoader(t))
	if report == nil {
		t.Fatal("want report")
	}
	if report.InputValid != handoffcheck.InputValidTrue || report.Error != "" || len(report.Entries) != 1 || !report.Entries[0].Valid {
		t.Fatalf("%+v", report)
	}
	if report.Entries[0].SchemaID != "schemas/report.schema.json" {
		t.Fatalf("schema id = %q", report.Entries[0].SchemaID)
	}
}

func TestBuildHandoffValidationReportInvalid(t *testing.T) {
	root := t.TempDir()
	report := buildHandoffValidationReport(context.Background(), root, map[string]handoffBinding{
		"summary": {LocalName: "summary", ProducerTask: "produce", SlotName: "report", MediaType: "application/json", SchemaPath: "schemas/report.schema.json"},
	}, publishHandoffSet(t, root, `{"verdict":"maybe"}`), testHandoffSchemaLoader(t))
	if report == nil {
		t.Fatal("want report")
	}
	if report.InputValid != handoffcheck.InputValidFalse || len(report.Entries) != 1 || report.Entries[0].Valid {
		t.Fatalf("%+v", report)
	}
	if len(report.Entries[0].Issues) == 0 || report.Entries[0].Issues[0].Code != handoffcheck.CodeSchemaViolation {
		t.Fatalf("issues = %+v", report.Entries[0].Issues)
	}
}

func TestBuildHandoffValidationReportUnknownOnLoaderError(t *testing.T) {
	root := t.TempDir()
	report := buildHandoffValidationReport(context.Background(), root, map[string]handoffBinding{
		"summary": {LocalName: "summary", ProducerTask: "produce", SlotName: "report", MediaType: "application/json", SchemaPath: "schemas/report.schema.json"},
	}, publishHandoffSet(t, root, `{"verdict":"pass"}`), func(string) (*handoffcheck.Schema, error) {
		return nil, os.ErrNotExist
	})
	if report == nil {
		t.Fatal("want report")
	}
	if report.InputValid != handoffcheck.InputValidUnknown || !strings.Contains(report.Error, "load schema") {
		t.Fatalf("%+v", report)
	}
}

func TestInvalidHandoffRerouteOffByDefault(t *testing.T) {
	producer := &handoffSetProducer{t: t, payloads: []string{`{"verdict":"maybe"}`}}
	consumer := &countingGoober{}
	r, _ := handoffRerouteRunner(t, producer, consumer, false)

	res, err := r.Start(context.Background(), handoffRerouteStartInput(t, "run-handoff-reroute-off"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	if producer.calls != 1 || consumer.calls != 1 {
		t.Fatalf("calls producer=%d consumer=%d, want 1/1", producer.calls, consumer.calls)
	}
}

func TestInvalidHandoffReroutesProducerForRetry(t *testing.T) {
	producer := &handoffSetProducer{t: t, payloads: []string{`{"verdict":"maybe"}`, `{"verdict":"pass"}`}}
	consumer := &countingGoober{}
	r, runsDir := handoffRerouteRunner(t, producer, consumer, true)

	res, err := r.Start(context.Background(), handoffRerouteStartInput(t, "run-handoff-reroute"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	events := readHandoffRunEvents(t, runsDir, "run-handoff-reroute")
	if producer.calls != 2 {
		t.Fatalf("producer calls = %d, want invalid handoff to reroute producer once; annotations=%v", producer.calls, handoffAnnotations(events))
	}
	if consumer.calls != 1 {
		t.Fatalf("consumer calls = %d, want consumer invoked only after valid handoff", consumer.calls)
	}
	if !hasHandoffRetryAnnotation(events, "consume", "produce", 1) {
		t.Fatalf("missing invalid handoff retry annotation in events: %+v", events)
	}
}

type handoffSetProducer struct {
	t        *testing.T
	rec      ArtifactRecorder
	payloads []string
	calls    int
}

func (p *handoffSetProducer) Run(ctx context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	p.calls++
	payload := p.payloads[min(p.calls-1, len(p.payloads)-1)]
	workspace := writeManifestWorkspace(p.t, payload)
	prepared, err := artifactset.Prepare(ctx, workspace, "manifest.json", func(_ string, data []byte) ([]byte, error) {
		return data, nil
	})
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	if err := prepared.Bind(env.ArtifactPublication, env.Attempt); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	artifacts, err := prepared.Publish(ctx, func(name, mediaType string, data []byte) (apiv1.ArtifactPointer, error) {
		ref, err := p.rec.RecordArtifact(name, data)
		if err != nil {
			return apiv1.ArtifactPointer{}, err
		}
		return apiv1.ArtifactPointer{
			Path: ref.Path, Digest: ref.Digest, Size: ref.Size,
			MediaType: mediaType, Integrity: ref.Integrity,
		}, nil
	})
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Artifacts: artifacts}, nil
}

type countingGoober struct {
	mu    sync.Mutex
	calls int
	envs  []apiv1.InvocationEnvelope
}

func (g *countingGoober) Invoke(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	g.envs = append(g.envs, env)
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (g *countingGoober) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

func handoffRerouteRunner(t *testing.T, producer *handoffSetProducer, consumer invoke.Goober, reroute bool) (*Runner, string) {
	t.Helper()
	r, runsDir := newTestRunnerWithDeterministic(t, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		producer.rec = rec
		return producer, nil
	}, nil)
	r.cfg.NewAgentic = func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
		return consumer, nil
	}
	r.cfg.HandoffSchemaLoader = testHandoffSchemaLoader(t)
	r.cfg.RerouteInvalidHandoffs = reroute
	return r, runsDir
}

func handoffRerouteStartInput(t *testing.T, runID string) StartInput {
	t.Helper()
	return StartInput{
		RunID:   runID,
		Machine: handoffRerouteMachine(t),
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	}
}

func handoffRerouteMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
		Start:    "produce",
		Tasks: []apiv1.Task{
			{
				Name: "produce", Type: apiv1.TaskDeterministic, Goal: "produce report",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "consume",
				ArtifactSlots: []apiv1.ArtifactSlot{{Name: "report", MediaType: "application/json", SchemaPath: "schemas/report.schema.json"}},
			},
			{Name: "consume", Type: apiv1.TaskAgentic, Goal: "consume report", Goober: "consumer", Next: workflow.TerminalComplete},
		},
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "handoff-reroute", Version: 1, DSLVersion: "3.1", Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile handoff reroute machine: %v", err)
	}
	return machine
}

func readHandoffRunEvents(t *testing.T, runsDir, runID string) []journal.Event {
	t.Helper()
	reader, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("open run journal: %v", err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatalf("read run journal: %v", err)
	}
	return events
}

func hasHandoffRetryAnnotation(events []journal.Event, consumer, producer string, attempt int) bool {
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != handoffValidationRetryAnnotationKind {
			continue
		}
		gotAttempt, _ := runnerInt(event.Runner["repassAttempt"])
		if event.Stage == consumer && event.Runner["producer"] == producer && gotAttempt == attempt {
			return true
		}
	}
	return false
}

func handoffAnnotations(events []journal.Event) []map[string]any {
	var annotations []map[string]any
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation {
			if kind, _ := event.Runner["kind"].(string); strings.HasPrefix(kind, "handoff.validation") {
				annotations = append(annotations, event.Runner)
			}
		}
	}
	return annotations
}

func TestInvalidHandoffEscalatesWhenRerouteBudgetExhausts(t *testing.T) {
	producer := &handoffSetProducer{t: t, payloads: []string{`{"verdict":"maybe"}`}}
	consumer := &countingGoober{}
	r, _ := handoffRerouteRunner(t, producer, consumer, true)

	input := handoffRerouteStartInput(t, "run-handoff-reroute-exhausted")
	input.RunControls = apiv1.RunControls{MaxRepasses: 1}
	res, err := r.Start(context.Background(), input)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseEscalated {
		t.Fatalf("phase = %q, want escalated", res.Phase)
	}
	if producer.calls != 2 || consumer.calls != 0 {
		t.Fatalf("calls producer=%d consumer=%d, want 2/0", producer.calls, consumer.calls)
	}
}

func TestInvalidHandoffRetryFromResultSurvivesJSONShape(t *testing.T) {
	result := apiv1.ResultEnvelope{
		Status: apiv1.ResultFailure,
		Error:  &apiv1.ErrorInfo{Code: invalidHandoffErrorCode},
		Outputs: map[string]interface{}{invalidHandoffOutputKey: map[string]interface{}{
			"consumer": "consume",
			"producer": "produce",
			"input":    "produce.report",
			"slot":     "report",
			"schemaId": "schemas/report.schema.json",
		}},
	}
	retry, ok := invalidHandoffRetryFromResult(result)
	if !ok || retry.Producer != "produce" || retry.Consumer != "consume" {
		t.Fatalf("retry = %+v ok=%v", retry, ok)
	}
}

func TestInvalidHandoffRetryAnnotationsSeedRepassBudget(t *testing.T) {
	events := []journal.Event{{
		Type:  journal.EventRunnerAnnotation,
		Stage: "consume",
		Runner: map[string]any{
			"kind":          handoffValidationRetryAnnotationKind,
			"target":        "produce",
			"repassAttempt": 2,
		},
	}}
	if got := gateRepassSeed(events)["handoff.validation:consume"]; got != 2 {
		t.Fatalf("gate seed = %d, want 2", got)
	}
	if got := targetRepassSeed(events)["produce"]; got != 2 {
		t.Fatalf("target seed = %d, want 2", got)
	}
}

func TestInvalidHandoffRetryAnnotationIsPendingRetryTarget(t *testing.T) {
	machine := handoffRerouteMachine(t)
	subject := apiv1.ResultEnvelope{
		Status: apiv1.ResultFailure,
		Error:  &apiv1.ErrorInfo{Code: invalidHandoffErrorCode},
		Outputs: map[string]interface{}{invalidHandoffOutputKey: invalidHandoffRetry{
			Consumer: "consume",
			Producer: "produce",
			Input:    "produce.report",
		}},
	}
	events := []journal.Event{
		{Type: journal.EventStageFinished, Stage: "consume", Status: string(apiv1.ResultFailure)},
		{Type: journal.EventRunnerAnnotation, Stage: "consume", Runner: map[string]any{
			"kind":   handoffValidationRetryAnnotationKind,
			"target": "produce",
		}},
	}
	target, ok := pendingRetryTarget(events, machine, "consume", subject)
	if !ok || target != "produce" {
		t.Fatalf("pendingRetryTarget = %q, %v; want produce,true", target, ok)
	}
}

func TestParallelInvalidHandoffRetryPrunesCurrentProducerPointers(t *testing.T) {
	exec := newParallelExec(apiv1.Parallel{
		Name: "p",
		Branches: []apiv1.Branch{{
			Name: "main", Start: "produce",
		}},
	})
	exec.recordCurrent(nil, []apiv1.ContextPointer{
		{Name: "produce.artifact[0]", Artifact: &apiv1.ArtifactPointer{Path: "old"}},
		{Name: "other.artifact[0]", Artifact: &apiv1.ArtifactPointer{Path: "keep"}},
	})
	exec.removeCurrentStageArtifactPointers("produce")
	got := exec.currentPointers(nil)
	if len(got) != 1 || got[0].Name != "other.artifact[0]" {
		t.Fatalf("pointers = %+v, want only other producer", got)
	}
	if current := exec.current(); current == nil || current.artifacts != 1 {
		t.Fatalf("current artifacts = %+v, want one retained artifact", current)
	}
}

// TestInvalidHandoffRetryIsDurableAcrossResume stops the run right after the
// reroute annotation is journaled and before the producer retry dispatches,
// then resumes it: the invalid artifact must stay pruned, so the consumer sees
// exactly the retried artifact and validates it as valid rather than unknown.
func TestInvalidHandoffRetryIsDurableAcrossResume(t *testing.T) {
	const runID = "run-handoff-reroute-resume"
	producer := &handoffSetProducer{t: t, payloads: []string{`{"verdict":"maybe"}`, `{"verdict":"pass"}`}}
	consumer := &countingGoober{}
	r, runsDir := handoffRerouteRunner(t, producer, consumer, true)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	load := r.cfg.HandoffSchemaLoader
	r.cfg.HandoffSchemaLoader = func(schemaPath string) (*handoffcheck.Schema, error) {
		stop()
		return load(schemaPath)
	}

	res, err := r.Start(ctx, handoffRerouteStartInput(t, runID))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseRunning || producer.calls != 1 || consumer.calls != 0 {
		t.Fatalf("interrupted phase=%q producer=%d consumer=%d, want running/1/0", res.Phase, producer.calls, consumer.calls)
	}
	events := readHandoffRunEvents(t, runsDir, runID)
	if !hasHandoffRetryAnnotation(events, "consume", "produce", 1) {
		t.Fatalf("missing retry annotation before interruption: %v", handoffAnnotations(events))
	}
	if got := countStageArtifactPointers(reconstructPointers(events, handoffRerouteMachine(t)), "produce"); got != 0 {
		t.Fatalf("reconstructed produce pointers = %d, want invalid artifact pruned", got)
	}

	r.cfg.HandoffSchemaLoader = load
	res, err = r.Resume(context.Background(), ResumeInput{
		RunID: runID, Machine: handoffRerouteMachine(t),
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Phase != journal.PhaseCompleted || producer.calls != 2 || consumer.calls != 1 {
		t.Fatalf("resumed phase=%q producer=%d consumer=%d, want completed/2/1", res.Phase, producer.calls, consumer.calls)
	}
	if got := countStageArtifactPointers(consumer.envs[0].ContextPointers, "produce"); got != oneHandoffSetPointers {
		t.Fatalf("consumer produce pointers = %d, want only the retried artifact: %+v", got, consumer.envs[0].ContextPointers)
	}
	if valid := lastHandoffInputValid(readHandoffRunEvents(t, runsDir, runID), "consume"); valid != string(handoffcheck.InputValidTrue) {
		t.Fatalf("consumer handoff verdict after resume = %q, want true", valid)
	}
}

func TestInvalidHandoffRerouteInsideParallelBranch(t *testing.T) {
	for _, tc := range []struct {
		name        string
		inherited   bool
		concurrency int32
		wantPhase   journal.RunPhase
		wantCalls   int
	}{
		{name: "sequential branch-local producer", concurrency: 1, wantPhase: journal.PhaseCompleted, wantCalls: 2},
		{name: "concurrent branch-local producer", concurrency: 2, wantPhase: journal.PhaseCompleted, wantCalls: 2},
		{name: "sequential inherited producer", inherited: true, concurrency: 1, wantPhase: journal.PhaseEscalated, wantCalls: 1},
		{name: "concurrent inherited producer", inherited: true, concurrency: 2, wantPhase: journal.PhaseEscalated, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runID := "run-handoff-parallel-" + strings.ReplaceAll(tc.name, " ", "-")
			producer := &handoffSetProducer{t: t, payloads: []string{`{"verdict":"maybe"}`, `{"verdict":"pass"}`}}
			consumer := &countingGoober{}
			r, runsDir := handoffRerouteRunner(t, producer, consumer, true)
			others := &countingGoober{}
			r.cfg.NewAgentic = func(name string, _ ArtifactRecorder, _ SecretRegistrar) (invoke.Goober, error) {
				if name == "consumer" {
					return consumer, nil
				}
				return others, nil
			}
			r.cfg.ScratchDir = t.TempDir()
			input := handoffRerouteStartInput(t, runID)
			input.Machine = handoffParallelMachine(t, tc.inherited, tc.concurrency)

			res, err := r.Start(context.Background(), input)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			events := readHandoffRunEvents(t, runsDir, runID)
			if res.Phase != tc.wantPhase || producer.calls != tc.wantCalls {
				t.Fatalf("phase=%q producer=%d, want %q/%d; annotations=%v", res.Phase, producer.calls, tc.wantPhase, tc.wantCalls, handoffAnnotations(events))
			}
			if tc.inherited {
				if consumer.calls != 0 || !hasUnsupportedHandoffAnnotation(events) {
					t.Fatalf("consumer=%d annotations=%v, want classified unsupported-topology escalation", consumer.calls, handoffAnnotations(events))
				}
				return
			}
			if consumer.calls != 1 || countStageArtifactPointers(consumer.envs[0].ContextPointers, "produce") != oneHandoffSetPointers {
				t.Fatalf("consumer calls=%d envs=%+v, want one call with only the retried artifact", consumer.calls, consumer.envs)
			}
		})
	}
}

func TestInvalidHandoffReplayPrunesBranchPointers(t *testing.T) {
	machine := handoffParallelMachine(t, false, 2)
	artifact := func(stage string) journal.Event {
		return journal.Event{Type: journal.EventStageFinished, Branch: 1, Stage: stage, Status: string(apiv1.ResultSuccess),
			Artifacts: []journal.Ref{{Path: "artifacts/" + stage, Digest: "sha256:" + stage}}}
	}
	events := []journal.Event{
		{Type: journal.EventParallelStarted, Parallel: "fan", Completeness: []journal.BranchOutcome{{Branch: 1, Name: "main"}, {Branch: 2, Name: "side"}}},
		{Type: journal.EventBranchStarted, Parallel: "fan", Branch: 1, BranchName: "main", Stage: "produce"},
		artifact("produce"),
		{Type: journal.EventStageFinished, Branch: 1, Stage: "consume", Status: string(apiv1.ResultFailure),
			Error: &journal.ErrorDetail{Code: invalidHandoffErrorCode}},
		{Type: journal.EventRunnerAnnotation, Branch: 1, Stage: "consume", Runner: map[string]any{
			"kind": handoffValidationRetryAnnotationKind, "target": "produce", "repassAttempt": 1,
		}},
	}
	if got := countStageArtifactPointers(reconstructPointers(events, machine), "produce"); got != 0 {
		t.Fatalf("reconstructed branch produce pointers = %d, want 0", got)
	}
	par, _ := pendingParallel(events, machine)
	if par == nil {
		t.Fatal("pendingParallel returned nil")
	}
	main := par.branch("main")
	if main.failed || main.artifacts != 0 || countStageArtifactPointers(main.pointers, "produce") != 0 {
		t.Fatalf("restored branch = %+v, want rerouted branch not failed and invalid artifact pruned", main)
	}
}

func TestInvalidHandoffParallelEscalationResumesAsEscalation(t *testing.T) {
	machine := handoffParallelMachine(t, true, 2)
	events := []journal.Event{
		{Type: journal.EventParallelStarted, Parallel: "fan", Completeness: []journal.BranchOutcome{{Branch: 1, Name: "main"}, {Branch: 2, Name: "side"}}},
		{Type: journal.EventStageFinished, Parallel: "fan", Branch: 1, Stage: "consume", Status: string(apiv1.ResultFailure),
			Error: &journal.ErrorDetail{Code: invalidHandoffErrorCode}},
		{Type: journal.EventRunnerAnnotation, Parallel: "fan", Branch: 1, Stage: "consume", Runner: map[string]any{
			"kind": handoffValidationRetryAnnotationKind, "escalated": true, "reason": invalidHandoffUnsupportedTopologyReason,
		}},
		{Type: journal.EventParallelFinished, Parallel: "fan", Target: workflow.TargetEscalate},
	}
	transition := pendingParallelTransition(events, machine)
	if transition == nil || transition.target != workflow.TargetEscalate || !transition.aggregate || transition.task != nil || transition.gate != nil {
		t.Fatalf("transition = %+v, want a direct escalation without a task or gate to replay", transition)
	}
}

func TestInvalidHandoffChargePreservesEvaluatorRepassCounters(t *testing.T) {
	// A fresh walk's evaluator lazily allocates counters the walk state never sees.
	ws := &walkState{gateEval: &gate.Evaluator{
		Attempts:       map[string]int{"review": 1},
		RepassAttempts: map[string]int{"produce": 1},
	}}
	budget := ws.repassBudget()
	retry := invalidHandoffRetry{Consumer: "consume", Producer: "produce", Input: "report"}
	decision := decideInvalidHandoff(&budget, "consume", retry, "", true, 2)
	ws.applyRepassBudget(budget)
	if decision.fields["repassAttempt"] != 2 {
		t.Fatalf("repassAttempt = %v, want 2 after the gate's earlier repass of the producer", decision.fields["repassAttempt"])
	}
	if ws.gateEval.Attempts["review"] != 1 {
		t.Fatalf("gate attempts = %v, want the review gate's attempt preserved", ws.gateEval.Attempts)
	}
}

func TestInvalidHandoffOutputsSurviveContinueOnErrorJournaling(t *testing.T) {
	result := apiv1.ResultEnvelope{
		Status:  apiv1.ResultFailure,
		Error:   &apiv1.ErrorInfo{Code: invalidHandoffErrorCode},
		Outputs: map[string]any{invalidHandoffOutputKey: map[string]any{"producer": "produce"}},
	}
	if _, ok := invalidHandoffRetryFromResult(apiv1.ResultEnvelope{Status: result.Status, Error: result.Error, Outputs: stageFinishedOutputs(result, true)}); !ok {
		t.Fatal("journaled continueOnError invalid-handoff outputs lost the reroute resume needs")
	}
}

func handoffParallelMachine(t *testing.T, inherited bool, concurrency int32) *workflow.Machine {
	t.Helper()
	produce := apiv1.Task{
		Name: "produce", Type: apiv1.TaskDeterministic, Goal: "produce report",
		Run:           &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
		ArtifactSlots: []apiv1.ArtifactSlot{{Name: "report", MediaType: "application/json", SchemaPath: "schemas/report.schema.json"}},
	}
	agentic := func(name, goober, next string) apiv1.Task {
		return apiv1.Task{Name: name, Type: apiv1.TaskAgentic, Goal: name, Goober: goober, Workspace: apiv1.WorkspaceScratch, Next: next}
	}
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
		Start:    "fan",
		Tasks: []apiv1.Task{
			produce,
			agentic("consume", "consumer", workflow.TargetJoin),
			agentic("side", "side", workflow.TargetJoin),
			agentic("collate", "collate", workflow.TerminalComplete),
		},
		Parallels: []apiv1.Parallel{{
			Name: "fan", FailurePolicy: apiv1.BranchContinueOnError, MaxConcurrentBranches: concurrency, Join: "collate",
			Branches: []apiv1.Branch{{Name: "main", Start: "consume"}, {Name: "side", Start: "side"}},
		}},
	}
	if inherited {
		spec.Start = "produce"
		spec.Tasks[0].Next = "fan"
	} else {
		spec.Tasks[0].Next = "consume"
		spec.Parallels[0].Branches[0].Start = "produce"
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "handoff-parallel", Version: 1, DSLVersion: "3.1", Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile handoff parallel machine: %v", err)
	}
	return machine
}

// oneHandoffSetPointers is the positional pointer count of one published
// artifact set from handoffSetProducer; a stale pre-retry set doubles it.
const oneHandoffSetPointers = 2

func countStageArtifactPointers(pointers []apiv1.ContextPointer, stage string) int {
	count := 0
	for _, pointer := range pointers {
		if strings.HasPrefix(pointer.Name, stage+".artifact[") {
			count++
		}
	}
	return count
}

func lastHandoffInputValid(events []journal.Event, stage string) string {
	valid := ""
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation && event.Stage == stage && event.Runner["kind"] == handoffValidationAnnotationKind {
			valid, _ = event.Runner["inputValid"].(string)
		}
	}
	return valid
}

func hasUnsupportedHandoffAnnotation(events []journal.Event) bool {
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation && event.Runner["kind"] == handoffValidationRetryAnnotationKind &&
			event.Runner["reason"] == invalidHandoffUnsupportedTopologyReason && event.Runner["escalated"] == true {
			return true
		}
	}
	return false
}
