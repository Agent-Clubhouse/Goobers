package runner

import (
	"context"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/handoffcheck"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// legacyResultProducer mimics a DSL 2.0 shell stage: a text log plus the
// declared .json result file, with no artifact-set manifest.
type legacyResultProducer struct {
	rec     ArtifactRecorder
	payload string
}

func (p *legacyResultProducer) Run(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	var artifacts []apiv1.ArtifactPointer
	for _, a := range []struct{ name, media, data string }{
		{"produce/stdout.log", "text/plain", "claimed\n"},
		{"produce/result", "application/json", p.payload},
	} {
		ref, err := p.rec.RecordArtifact(a.name, []byte(a.data))
		if err != nil {
			return apiv1.ResultEnvelope{}, err
		}
		artifacts = append(artifacts, apiv1.ArtifactPointer{Path: ref.Path, Digest: ref.Digest, Size: ref.Size, MediaType: a.media, Integrity: ref.Integrity})
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Artifacts: artifacts}, nil
}

func legacyHandoffMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
		Start:    "produce",
		Tasks: []apiv1.Task{
			{
				Name: "produce", Type: apiv1.TaskDeterministic, Goal: "claim an item",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "consume",
			},
			{Name: "consume", Type: apiv1.TaskAgentic, Goal: "implement the item", Goober: "consumer", Next: workflow.TerminalComplete},
		},
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "legacy-handoff", Version: 1, DSLVersion: "2.0", Spec: spec})
	if err != nil {
		t.Fatalf("compile legacy handoff machine: %v", err)
	}
	return machine
}

func runLegacyHandoff(t *testing.T, runID, payload string, schemas ResultHandoffSchemas) []map[string]any {
	t.Helper()
	producer := &legacyResultProducer{payload: payload}
	r, runsDir := newTestRunnerWithDeterministic(t, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		producer.rec = rec
		return producer, nil
	}, nil)
	consumer := &countingGoober{}
	r.cfg.NewAgentic = func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) { return consumer, nil }
	r.cfg.HandoffSchemaLoader = testHandoffSchemaLoader(t)
	r.cfg.ResultHandoffSchemas = schemas
	input := handoffRerouteStartInput(t, runID)
	input.Machine = legacyHandoffMachine(t)
	res, err := r.Start(context.Background(), input)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted || consumer.calls != 1 {
		t.Fatalf("phase=%q consumer calls=%d, want completed/1", res.Phase, consumer.calls)
	}
	return handoffAnnotations(readHandoffRunEvents(t, runsDir, runID))
}

func TestLegacyResultHandoffSchemaGivesAuthoritativeInputValid(t *testing.T) {
	bound := ResultHandoffSchemas{"legacy-handoff": {"produce": "schemas/report.schema.json"}}
	for _, tc := range []struct {
		name, payload string
		want          handoffcheck.InputValid
	}{
		{"valid", `{"verdict":"pass"}`, handoffcheck.InputValidTrue},
		{"invalid", `{"verdict":"maybe"}`, handoffcheck.InputValidFalse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			annotations := runLegacyHandoff(t, "run-legacy-"+tc.name, tc.payload, bound)
			if len(annotations) != 1 || annotations[0]["inputValid"] != string(tc.want) {
				t.Fatalf("annotations = %+v, want one inputValid=%s", annotations, tc.want)
			}
		})
	}
}

func TestLegacyResultHandoffBoundToOtherWorkflowStaysUnvalidated(t *testing.T) {
	schemas := ResultHandoffSchemas{"implementation": {"produce": "schemas/report.schema.json"}}
	if annotations := runLegacyHandoff(t, "run-legacy-unbound", `{"verdict":"pass"}`, schemas); len(annotations) != 0 {
		t.Fatalf("annotations = %+v, want none", annotations)
	}
}

func TestLegacyResultHandoffAmbiguousPayloadIsUnknown(t *testing.T) {
	root := t.TempDir()
	var pointers []apiv1.ContextPointer
	for i, payload := range []string{`{"verdict":"pass"}`, `{"verdict":"maybe"}`} {
		pointer, err := apiv1.WriteArtifact(root, "artifacts/produce/result"+string(rune('a'+i)), []byte(payload), "application/json")
		if err != nil {
			t.Fatal(err)
		}
		pointers = append(pointers, contextPointersFor("produce", []apiv1.ArtifactPointer{pointer})...)
	}
	pointers[1].Name = "produce.artifact[1]"
	binding, _ := resultHandoffBinding(apiv1.Task{Name: "produce"}, "schemas/report.schema.json")
	report := buildHandoffValidationReport(context.Background(), root, map[string]handoffBinding{binding.LocalName: binding}, pointers, testHandoffSchemaLoader(t))
	if report == nil || report.InputValid != handoffcheck.InputValidUnknown || len(report.Entries) != 0 || report.Error == "" {
		t.Fatalf("report = %+v, want unknown with an error and no verdict", report)
	}
}
