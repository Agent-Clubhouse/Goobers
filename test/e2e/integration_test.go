package e2e

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/codes"
	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/gooberruntime"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/scheduler"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/providers"
)

// TestWalkingSkeletonWiredPath drives a single backlog item through the REAL
// wired control plane — bootstrap (config → registry) → scheduler (readiness,
// claim, span) → engine (state machine) → goober runtime → telemetry — and
// asserts the run completes with a result and emits the run + task trace.
//
// Two seams stand in for infrastructure that cannot run in CI, exactly as a
// real run would use them through the same interfaces: the goober harness is a
// fake invoke.Goober (the external Copilot boundary), and Temporal execution
// uses the SDK test environment instead of a live server. Everything between —
// config load, registry, scheduler dispatch, engine state machine, envelope
// flow, telemetry spans — is the production code path.
func TestWalkingSkeletonWiredPath(t *testing.T) {
	loaded, err := bootstrap.LoadAndRegister("../fixtures/e2e/walking-skeleton", "")
	if err != nil {
		t.Fatalf("bootstrap load: %v", err)
	}
	gaggle := loaded.Gaggles[0]
	workflow := loaded.Workflows[0]

	exporter := telemetry.NewMemoryExporter()
	tel, err := telemetry.New(context.Background(), telemetry.Config{
		ServiceName:  "e2e-wired",
		SpanExporter: exporter,
	})
	if err != nil {
		t.Fatalf("telemetry: %v", err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })

	// The REAL goober runtime (M8), exercising its true Invoke path — environment
	// prep, instruction context, harness call, result validation — with only the
	// external boundaries faked: the workspace preparer (cluster/repo clone) and
	// the harness (Copilot). The runtime is wrapped to emit the agentic task span
	// (QA-2: runtime owns the task span; the v1 runtime does not emit telemetry
	// itself yet).
	runtime := gooberruntime.New(gooberruntime.Options{
		Preparer: fakePreparer{dir: t.TempDir()},
		Harness:  fakeHarness{},
	})
	goober := &taskSpanRuntime{inner: runtime, tel: tel}
	starter := &runEnvStarter{t: t, tel: tel, goober: goober}

	sched, err := loaded.SchedulerFor(gaggle.Name, bootstrap.SchedulerDeps{
		Starter:   starter,
		Telemetry: tel,
	})
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}

	item := providers.WorkItem{
		Provider: providers.ProviderGitHub,
		ID:       "101",
		Title:    "Add walking skeleton smoke path",
		Labels:   []string{"goober:ready"},
		URL:      "https://github.com/acme/web/issues/101",
	}
	decision, err := sched.Dispatch(context.Background(), scheduler.Event{
		WorkflowName: workflow.Name,
		Item:         &item,
		Reason:       "backlog-item",
		DedupeKey:    "github:101",
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !decision.Started {
		t.Fatalf("expected the wired path to start a run, got %+v", decision)
	}

	// The run flowed through the engine to the goober and produced a result.
	if starter.result.Status != engine.StatusCompleted {
		t.Fatalf("run status = %q, want completed", starter.result.Status)
	}
	if got := starter.result.Outputs["implement"].Status; got != apiv1.ResultSuccess {
		t.Fatalf("implement result = %q, want success", got)
	}
	if starter.invoked.WorkflowID != workflow.Name || starter.invoked.Gaggle != gaggle.Name {
		t.Fatalf("invocation envelope = %#v, want workflow %q gaggle %q", starter.invoked, workflow.Name, gaggle.Name)
	}
	if starter.invoked.Item == nil || starter.invoked.Item.ID != item.ID {
		t.Fatalf("invocation item = %#v, want %s", starter.invoked.Item, item.ID)
	}

	// Telemetry: the run trace carries the task span nested under the run span,
	// and the scheduler recorded its evaluate span.
	if err := tel.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	spans := exporter.Spans()
	run := findSpan(t, spans, "run/"+workflow.Name)
	task := findSpan(t, spans, "task/implement")
	_ = findSpan(t, spans, "scheduler/evaluate")

	if task.SpanContext().TraceID() != run.SpanContext().TraceID() {
		t.Fatalf("task trace %s != run trace %s", task.SpanContext().TraceID(), run.SpanContext().TraceID())
	}
	if task.Parent().SpanID() != run.SpanContext().SpanID() {
		t.Fatalf("task parent = %s, want run span %s", task.Parent().SpanID(), run.SpanContext().SpanID())
	}
	if task.Status().Code != codes.Ok {
		t.Fatalf("task status = %s, want OK", task.Status().Code)
	}
}

// runEnvStarter is the e2e engine.Starter: it executes engine runs on a Temporal
// test environment, wrapping each in a telemetry run span so the goober's task
// span nests under it. In production this role is engine.TemporalStarter against
// a live Temporal server (cmd/goober-runtime hosts the worker).
type runEnvStarter struct {
	t       *testing.T
	tel     *telemetry.Client
	goober  *taskSpanRuntime
	result  engine.RunResult
	invoked apiv1.InvocationEnvelope
}

func (s *runEnvStarter) Start(_ context.Context, in engine.RunInput) (engine.StartResult, error) {
	// The run is the trace (run=trace); start it from a fresh context so it roots
	// its own trace with a fresh OTel run id.
	runID, err := telemetry.NewRunID()
	if err != nil {
		return engine.StartResult{}, err
	}
	runCtx, runSpan, err := s.tel.StartRun(context.Background(), telemetry.RunAttributes{
		Gaggle:       in.Gaggle,
		WorkflowID:   in.WorkflowName,
		RunID:        runID,
		Trigger:      string(apiv1.TriggerBacklogItem),
		ItemID:       itemID(in),
		ItemProvider: itemProvider(in),
	})
	if err != nil {
		return engine.StartResult{}, err
	}
	s.goober.runCtx = runCtx
	s.goober.runTraceID = runID
	s.goober.onInvoke = func(env apiv1.InvocationEnvelope) { s.invoked = env }

	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterActivity(&engine.Activities{Goober: s.goober})
	env.ExecuteWorkflow(engine.Run, in)

	if !env.IsWorkflowCompleted() {
		runSpan.Fail(errors.New("workflow did not complete"))
		return engine.StartResult{}, errors.New("workflow did not complete")
	}
	if werr := env.GetWorkflowError(); werr != nil {
		runSpan.Fail(werr)
		return engine.StartResult{}, werr
	}
	if rerr := env.GetWorkflowResult(&s.result); rerr != nil {
		runSpan.Fail(rerr)
		return engine.StartResult{}, rerr
	}
	runSpan.Succeed("walking skeleton run complete")
	return engine.StartResult{RunID: in.RunID}, nil
}

// taskSpanRuntime wraps the real goober runtime to emit the agentic task span
// (QA-2 span-ownership: the runtime owns the task span) under the run context,
// then delegates to the wrapped runtime's real Invoke/Review. The OTel run-trace
// id (hex) is distinct from env.RunID (the deterministic Temporal workflow id for
// dedup), so the task span links to the run via the run-trace id from the starter.
type taskSpanRuntime struct {
	inner      invoke.Goober
	tel        *telemetry.Client
	runCtx     context.Context
	runTraceID string
	onInvoke   func(apiv1.InvocationEnvelope)
}

func (g *taskSpanRuntime) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	if g.onInvoke != nil {
		g.onInvoke(env)
	}
	_, span, err := g.tel.StartTask(g.runCtx, telemetry.TaskAttributes{
		Gaggle:       env.Gaggle,
		WorkflowID:   env.WorkflowID,
		RunID:        g.runTraceID,
		TaskID:       "implement",
		TaskType:     string(apiv1.TaskAgentic),
		GooberID:     "coder",
		ItemID:       itemID2(env),
		ItemProvider: itemProvider2(env),
	})
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	res, ierr := g.inner.Invoke(ctx, env)
	if ierr != nil {
		span.Fail(ierr)
		return apiv1.ResultEnvelope{}, ierr
	}
	span.Succeed("goober completed")
	return res, nil
}

func (g *taskSpanRuntime) Review(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return g.inner.Review(ctx, env)
}

// fakeHarness stands in for the external Copilot agent harness (un-CI-able). It
// returns a canned success result / pass verdict; the runtime's real
// orchestration + result validation run around it.
type fakeHarness struct{}

func (fakeHarness) Invoke(context.Context, gooberruntime.HarnessRequest) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{
		Status:    apiv1.ResultSuccess,
		Summary:   "walking skeleton goober completed one backlog item",
		Outputs:   map[string]interface{}{"pullRequest": "https://github.com/acme/web/pull/1"},
		Artifacts: []apiv1.Artifact{{Type: "pull-request", URI: "https://github.com/acme/web/pull/1", Label: "PR"}},
	}, nil
}

func (fakeHarness) Review(context.Context, gooberruntime.HarnessRequest) (apiv1.Verdict, error) {
	return apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "looks good"}, nil
}

// fakePreparer stands in for the workspace/repo-clone preparer (cluster boundary).
type fakePreparer struct{ dir string }

func (p fakePreparer) Prepare(context.Context, apiv1.InvocationEnvelope) (gooberruntime.ExecutionEnvironment, error) {
	return gooberruntime.ExecutionEnvironment{WorkspaceDir: p.dir, RepoDir: p.dir}, nil
}

func itemID(in engine.RunInput) string {
	if in.Item != nil {
		return in.Item.ID
	}
	return ""
}

func itemProvider(in engine.RunInput) string {
	if in.Item != nil {
		return string(in.Item.Provider)
	}
	return ""
}

func itemID2(env apiv1.InvocationEnvelope) string {
	if env.Item != nil {
		return env.Item.ID
	}
	return ""
}

func itemProvider2(env apiv1.InvocationEnvelope) string {
	if env.Item != nil {
		return string(env.Item.Provider)
	}
	return ""
}
