package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.temporal.io/sdk/testsuite"
	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/telemetry"
	wf "github.com/goobers/goobers/internal/workflow"
)

const fixtureRoot = "../fixtures/e2e/walking-skeleton"

func TestWalkingSkeletonFixtureValidatesAndCompiles(t *testing.T) {
	v, err := validate.New()
	if err != nil {
		t.Fatalf("validator: %v", err)
	}
	report, err := v.ValidateDir(fixtureRoot)
	if err != nil {
		t.Fatalf("validate fixture: %v", err)
	}
	if report.HasErrors() {
		t.Fatalf("walking-skeleton fixture has validation errors: %#v", report.Issues)
	}

	gaggle := loadYAML[apiv1.Gaggle](t, "gaggles/acme-web/gaggle.yaml")
	goober := loadYAML[apiv1.Goober](t, "gaggles/acme-web/goobers/coder.yaml")
	workflow := loadYAML[apiv1.Workflow](t, "gaggles/acme-web/workflows/default-implement.yaml")

	if goober.Spec.Gaggle != gaggle.Name {
		t.Fatalf("goober gaggle = %q, want %q", goober.Spec.Gaggle, gaggle.Name)
	}
	if len(workflow.Spec.Tasks) != 1 || workflow.Spec.Tasks[0].Goober != goober.Name {
		t.Fatalf("workflow tasks = %#v, want single coder task", workflow.Spec.Tasks)
	}
	if _, err := wf.Compile(wf.Definition{Name: workflow.Name, Version: 1, Spec: workflow.Spec}); err != nil {
		t.Fatalf("compile workflow: %v", err)
	}
}

func TestWalkingSkeletonSingleItemRunEmitsTrace(t *testing.T) {
	gaggle := loadYAML[apiv1.Gaggle](t, "gaggles/acme-web/gaggle.yaml")
	workflow := loadYAML[apiv1.Workflow](t, "gaggles/acme-web/workflows/default-implement.yaml")

	runID, err := telemetry.NewRunID()
	if err != nil {
		t.Fatalf("new run id: %v", err)
	}
	item := &apiv1.BacklogItem{
		ID:       "101",
		Provider: apiv1.ProviderGitHub,
		Title:    "Add walking skeleton smoke path",
		Labels:   []string{"goober:ready"},
		URL:      "https://github.com/acme/web/issues/101",
	}

	registry := engine.NewRegistry()
	if _, err := registry.Register(workflow.Name, workflow.Spec); err != nil {
		t.Fatalf("register workflow: %v", err)
	}
	input, err := registry.StartInput(workflow.Name, engine.StartSpec{
		RunID:   runID,
		Gaggle:  gaggle.Name,
		RepoRef: gaggle.Spec.Project,
		Item:    item,
	})
	if err != nil {
		t.Fatalf("start input: %v", err)
	}

	exporter := telemetry.NewMemoryExporter()
	tel, err := telemetry.New(context.Background(), telemetry.Config{
		ServiceName:  "e2e-walking-skeleton",
		SpanExporter: exporter,
	})
	if err != nil {
		t.Fatalf("telemetry client: %v", err)
	}
	t.Cleanup(func() {
		if err := tel.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown telemetry: %v", err)
		}
	})

	runCtx, runSpan, err := tel.StartRun(context.Background(), telemetry.RunAttributes{
		Gaggle:       input.Gaggle,
		WorkflowID:   input.WorkflowName,
		RunID:        input.RunID,
		ItemID:       item.ID,
		ItemProvider: string(item.Provider),
		Trigger:      string(apiv1.TriggerBacklogItem),
	})
	if err != nil {
		t.Fatalf("start run span: %v", err)
	}

	var invoked apiv1.InvocationEnvelope
	goober := &tracingGoober{
		tel:    tel,
		runCtx: runCtx,
		onInvoke: func(env apiv1.InvocationEnvelope) {
			invoked = env
			if env.RunID != runID || env.WorkflowID != workflow.Name || env.Gaggle != gaggle.Name {
				t.Fatalf("invocation envelope identifiers = %#v", env)
			}
			if env.Item == nil || env.Item.ID != item.ID {
				t.Fatalf("invocation item = %#v, want %s", env.Item, item.ID)
			}
		},
	}

	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterActivity(&engine.Activities{Goober: goober})
	env.ExecuteWorkflow(engine.Run, input)
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result engine.RunResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if result.Status != engine.StatusCompleted {
		t.Fatalf("run status = %q, want %q", result.Status, engine.StatusCompleted)
	}
	if got := result.Outputs["implement"].Status; got != apiv1.ResultSuccess {
		t.Fatalf("implement output = %q, want %q", got, apiv1.ResultSuccess)
	}
	if invoked.TaskID != runID+":implement" {
		t.Fatalf("task id = %q, want %q", invoked.TaskID, runID+":implement")
	}

	runSpan.Succeed("walking skeleton completed")
	if err := tel.Flush(context.Background()); err != nil {
		t.Fatalf("flush telemetry: %v", err)
	}

	spans := exporter.Spans()
	run := findSpan(t, spans, "run/default-implement")
	task := findSpan(t, spans, "task/implement")
	expectedTraceID, err := trace.TraceIDFromHex(runID)
	if err != nil {
		t.Fatalf("trace id: %v", err)
	}
	if run.SpanContext().TraceID() != expectedTraceID || task.SpanContext().TraceID() != expectedTraceID {
		t.Fatalf("trace IDs run=%s task=%s want %s", run.SpanContext().TraceID(), task.SpanContext().TraceID(), expectedTraceID)
	}
	if task.Parent().SpanID() != run.SpanContext().SpanID() {
		t.Fatalf("task parent = %s, want run span %s", task.Parent().SpanID(), run.SpanContext().SpanID())
	}
	if task.Status().Code != codes.Ok {
		t.Fatalf("task status = %s, want OK", task.Status().Code)
	}
}

type tracingGoober struct {
	tel      *telemetry.Client
	runCtx   context.Context
	onInvoke func(apiv1.InvocationEnvelope)
}

func (g *tracingGoober) Invoke(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	if g.onInvoke != nil {
		g.onInvoke(env)
	}
	_, span, err := g.tel.StartTask(g.runCtx, telemetry.TaskAttributes{
		Gaggle:       env.Gaggle,
		WorkflowID:   env.WorkflowID,
		RunID:        env.RunID,
		TaskID:       "implement",
		TaskType:     string(apiv1.TaskAgentic),
		GooberID:     "coder",
		ItemID:       env.Item.ID,
		ItemProvider: string(env.Item.Provider),
	})
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	span.Succeed("goober completed")
	return apiv1.ResultEnvelope{
		Status:  apiv1.ResultSuccess,
		Summary: "walking skeleton goober completed one backlog item",
		Outputs: map[string]interface{}{
			"pullRequest": "https://github.com/acme/web/pull/1",
		},
	}, nil
}

func (g *tracingGoober) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

func loadYAML[T any](t *testing.T, rel string) T {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureRoot, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	var out T
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode %s: %v", rel, err)
	}
	return out
}

func findSpan(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, span := range spans {
		if span.Name() == name {
			return span
		}
	}
	t.Fatalf("span %q not found; got %v", name, spanNames(spans))
	return nil
}

func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	names := make([]string, 0, len(spans))
	for _, span := range spans {
		names = append(names, span.Name())
	}
	return names
}
