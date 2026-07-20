package engine

import (
	"context"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	wf "github.com/goobers/goobers/internal/workflow"
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
	var gotLimits apiv1.Limits
	inv.review = func(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
		gotLimits = env.Limits
		return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
	}
	env.RegisterActivity(&Activities{Goober: inv})

	spec := gatedSpec()
	spec.Gates[0].Agentic.TimeoutSeconds = 37
	env.ExecuteWorkflow(Run, runInput("gated", spec))

	var res RunResult
	_ = env.GetWorkflowResult(&res)
	if res.Status != StatusCompleted {
		t.Errorf("status = %q, want completed", res.Status)
	}
	if gotLimits.MaxDurationSeconds != 37 {
		t.Errorf("review limits = %+v, want 37s duration", gotLimits)
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
			{
				Name: "lint", Type: apiv1.TaskDeterministic, Goal: "run lint",
				Run:            &apiv1.DeterministicRun{Command: []string{"make", "lint"}, Env: map[string]string{"CI": "true"}},
				TimeoutSeconds: 25,
				Limits:         &apiv1.Limits{MaxTokens: 1000, MaxCostUSD: 1.25},
			},
		},
	}
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var gotCmd []string
	var gotEnv map[string]string
	var gotLimits apiv1.Limits
	env.RegisterActivity(&Activities{Det: &fakeRunner{
		run: func(_ context.Context, invocation apiv1.InvocationEnvelope, r apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
			gotCmd = r.Command
			gotEnv = r.Env
			gotLimits = invocation.Limits
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
	if gotEnv["CI"] != "true" {
		t.Errorf("runner got env %v, want CI=true", gotEnv)
	}
	if gotLimits.MaxDurationSeconds != 25 || gotLimits.MaxTokens != 1000 || gotLimits.MaxCostUSD != 1.25 {
		t.Errorf("runner got limits %+v, want declared task limits", gotLimits)
	}
}

func TestCIPollTaskReceivesDownstreamGateCadence(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
		Start:    "ci-poll",
		Tasks: []apiv1.Task{{
			Name: "ci-poll", Type: apiv1.TaskDeterministic, Goal: "poll CI",
			Run:          &apiv1.DeterministicRun{Command: []string{"goobers", "ci-poll"}},
			Inputs:       map[string]string{"kind": "ci-poll", "prNumber": "42"},
			Capabilities: []string{"github:pr:write"},
			Next:         "ci-gate",
		}},
		Gates: []apiv1.Gate{{
			Name:      "ci-gate",
			Evaluator: apiv1.EvaluatorAutomated,
			Automated: &apiv1.AutomatedGate{Check: "ci-status", PollIntervalSeconds: 9},
			Branches: map[string]string{
				"pass":    wf.TerminalComplete,
				"fail":    wf.TargetAbort,
				"timeout": wf.TargetEscalate,
			},
		}},
	}
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	var gotInterval interface{}
	env.RegisterActivity(&Activities{
		Det: &fakeRunner{run: func(_ context.Context, invocation apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
			gotInterval = invocation.Inputs["pollIntervalSeconds"]
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
		}},
		Auto: &fixtureAuto{ciStatus: "passing"},
	})

	env.ExecuteWorkflow(Run, runInput("ci-cadence", spec))

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if gotInterval != "9s" {
		t.Fatalf("ci-poll cadence input = %v, want 9s from downstream gate", gotInterval)
	}
}

func TestHumanGateRejectedBeforeSignal(t *testing.T) {
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
				Branches:  map[string]string{"pass": wf.TerminalComplete, "reject": wf.TargetAbort},
			},
		},
	}
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.ExecuteWorkflow(Run, runInput("human", spec))

	err := env.GetWorkflowError()
	const want = "human gates ship with durable pause/resume (#168/#465); until then use an automated gate or remove this block"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("workflow error = %v, want actionable human-gate rejection", err)
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
