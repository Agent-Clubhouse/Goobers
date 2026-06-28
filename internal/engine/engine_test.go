package engine

import (
	"context"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// fakeInvoker is a test double for the stubbed GooberInvoker boundary.
type fakeInvoker struct {
	invoke func(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error)
	review func(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error)
}

func (f *fakeInvoker) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return f.invoke(ctx, env)
}

func (f *fakeInvoker) Review(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return f.review(ctx, env)
}

type fakeRunner struct {
	run func(ctx context.Context, env apiv1.InvocationEnvelope, r apiv1.DeterministicRun) (apiv1.ResultEnvelope, error)
}

func (f *fakeRunner) Run(ctx context.Context, env apiv1.InvocationEnvelope, r apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	return f.run(ctx, env, r)
}

func runInput(name string, spec apiv1.WorkflowSpec) RunInput {
	return RunInput{
		RunID:        "run-" + name,
		Gaggle:       "web",
		WorkflowName: name,
		Version:      1,
		Spec:         spec,
		RepoRef:      apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
	}
}

func successInvoker() *fakeInvoker {
	return &fakeInvoker{
		invoke: func(_ context.Context, _ apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "done"}, nil
		},
	}
}

// TestLinearFlowCompletes: a single agentic task runs to a terminal state.
func TestLinearFlowCompletes(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Goober: successInvoker()})

	env.ExecuteWorkflow(Run, runInput("linear", linearSpec()))

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var res RunResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("get result: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Errorf("status = %q, want completed", res.Status)
	}
	if got := res.Outputs["implement"].Status; got != apiv1.ResultSuccess {
		t.Errorf("implement output status = %q, want success", got)
	}
}

// TestGateBlocksRun: an agentic gate returns "fail", which the definition routes
// to @abort, so the run ends blocked.
func TestGateBlocksRun(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	inv := successInvoker()
	inv.review = func(_ context.Context, _ apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
		return apiv1.Verdict{Decision: apiv1.VerdictFail, Summary: "rejected"}, nil
	}
	env.RegisterActivity(&Activities{Goober: inv})

	env.ExecuteWorkflow(Run, runInput("gated", gatedSpec()))

	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow did not complete cleanly: %v", env.GetWorkflowError())
	}
	var res RunResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("get result: %v", err)
	}
	if res.Status != StatusBlocked {
		t.Errorf("status = %q, want blocked", res.Status)
	}
	if res.FinalState != "review" {
		t.Errorf("finalState = %q, want review", res.FinalState)
	}
	// The task still ran before the gate blocked.
	if _, ok := res.Outputs["implement"]; !ok {
		t.Error("expected the implement task to have run before the gate")
	}
}

// TestGatePassContinues: an agentic gate returning "pass" completes the run.
func TestGatePassContinues(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	inv := successInvoker()
	inv.review = func(_ context.Context, _ apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
		return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
	}
	env.RegisterActivity(&Activities{Goober: inv})

	env.ExecuteWorkflow(Run, runInput("gated", gatedSpec()))

	var res RunResult
	_ = env.GetWorkflowResult(&res)
	if res.Status != StatusCompleted {
		t.Errorf("status = %q, want completed", res.Status)
	}
}

// TestGateLoopThenPass: a gate routes "needs-changes" back to the task, then
// passes on the second review — exercising a cycle without runaway.
func TestGateLoopThenPass(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	reviews := 0
	inv := successInvoker()
	inv.review = func(_ context.Context, _ apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
		reviews++
		if reviews == 1 {
			return apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges}, nil
		}
		return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
	}
	env.RegisterActivity(&Activities{Goober: inv})

	env.ExecuteWorkflow(Run, runInput("gated", gatedSpec()))

	var res RunResult
	_ = env.GetWorkflowResult(&res)
	if res.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", res.Status)
	}
	if reviews != 2 {
		t.Errorf("reviews = %d, want 2 (looped once)", reviews)
	}
}

// TestDeterministicTask: a deterministic task runs via the DeterministicRunner.
func TestDeterministicTask(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: "@every 1h"}},
		Start:    "lint",
		Tasks: []apiv1.Task{
			{Name: "lint", Type: apiv1.TaskDeterministic, Goal: "run lint", Run: &apiv1.DeterministicRun{Command: []string{"make", "lint"}}},
		},
	}
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var gotCmd []string
	env.RegisterActivity(&Activities{Det: &fakeRunner{
		run: func(_ context.Context, _ apiv1.InvocationEnvelope, r apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
			gotCmd = r.Command
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
		},
	}})

	env.ExecuteWorkflow(Run, runInput("det", spec))

	var res RunResult
	_ = env.GetWorkflowResult(&res)
	if res.Status != StatusCompleted {
		t.Errorf("status = %q, want completed", res.Status)
	}
	if len(gotCmd) != 2 || gotCmd[0] != "make" {
		t.Errorf("runner got command %v, want [make lint]", gotCmd)
	}
}

// TestHumanGateSignal: a human gate proceeds when its decision signal arrives.
func TestHumanGateSignal(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "implement", Next: "approve"},
		},
		Gates: []apiv1.Gate{
			{
				Name:      "approve",
				Evaluator: apiv1.EvaluatorHuman,
				Human:     &apiv1.HumanGate{},
				Branches:  map[string]string{"pass": TerminalComplete, "reject": TargetAbort},
			},
		},
	}
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Goober: successInvoker()})
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(HumanGateSignal("approve"), "pass")
	}, time.Millisecond)

	env.ExecuteWorkflow(Run, runInput("human", spec))

	var res RunResult
	_ = env.GetWorkflowResult(&res)
	if res.Status != StatusCompleted {
		t.Errorf("status = %q, want completed", res.Status)
	}
}

// TestAgenticTaskNotConfiguredErrors: an agentic task with no GooberInvoker wired
// surfaces a clear error rather than panicking.
func TestAgenticTaskNotConfiguredErrors(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{}) // no Goober

	env.ExecuteWorkflow(Run, runInput("linear", linearSpec()))

	if env.GetWorkflowError() == nil {
		t.Fatal("expected a workflow error when the invoker is not configured")
	}
}
