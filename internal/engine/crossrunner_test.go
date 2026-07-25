package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	wf "github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

// scriptedStages is an invoke.Deterministic scripted per stage name and call
// index, usable behind BOTH runners — which is what makes the cross-runner
// outcome table below a genuine same-fixture comparison. The last scripted
// result for a stage repeats; an unscripted stage succeeds.
type scriptedStages struct {
	mu      sync.Mutex
	results map[string][]apiv1.ResultEnvelope
	calls   map[string]int
}

func (s *scriptedStages) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stage := env.TaskID[strings.Index(env.TaskID, ":")+1:]
	if s.calls == nil {
		s.calls = map[string]int{}
	}
	n := s.calls[stage]
	s.calls[stage] = n + 1
	script := s.results[stage]
	if len(script) == 0 {
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}
	if n >= len(script) {
		n = len(script) - 1
	}
	return script[n], nil
}

func (s *scriptedStages) callCounts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.calls))
	for k, v := range s.calls {
		out[k] = v
	}
	return out
}

// countingAutomated wraps a real automated evaluator and counts evaluations.
type countingAutomated struct {
	mu    sync.Mutex
	inner invoke.Automated
	calls int
}

func (c *countingAutomated) Evaluate(ctx context.Context, g apiv1.AutomatedGate, env apiv1.InvocationEnvelope) (string, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.inner.Evaluate(ctx, g, env)
}

func (c *countingAutomated) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func crTask(name, next string) apiv1.Task {
	return apiv1.Task{
		Name: name, Type: apiv1.TaskDeterministic, Goal: name,
		Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
		Next: next,
	}
}

func crGate(name string, branches map[string]string) apiv1.Gate {
	return apiv1.Gate{
		Name: name, Evaluator: apiv1.EvaluatorAutomated,
		Automated: &apiv1.AutomatedGate{Check: "status-equals"},
		Branches:  branches,
	}
}

func crSpec(start string, tasks []apiv1.Task, gates []apiv1.Gate) apiv1.WorkflowSpec {
	return apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    start,
		Tasks:    tasks,
		Gates:    gates,
	}
}

func failureResult(code, message string) apiv1.ResultEnvelope {
	return apiv1.ResultEnvelope{Status: apiv1.ResultFailure, Error: &apiv1.ErrorInfo{Code: code, Message: message}}
}

// statusForPhase maps the local runner's terminal phase onto the engine's
// RunResult status vocabulary — the same mapping the projection (#629) will
// commit to.
func statusForPhase(t *testing.T, p journal.RunPhase) string {
	t.Helper()
	switch p {
	case journal.PhaseCompleted:
		return StatusCompleted
	case journal.PhaseAborted:
		return StatusBlocked
	case journal.PhaseEscalated:
		return StatusEscalated
	case journal.PhaseFailed:
		return StatusFailed
	}
	t.Fatalf("unexpected local terminal phase %q", p)
	return ""
}

// TestCrossRunnerTerminalOutcomeParity is #624's acceptance table: identical
// definitions, identical scripted stage results, and the SAME real automated
// evaluator (gate.NewAutomatedEvaluator over the flattened subject
// status/outputs) walked through the local runner and the engine must yield
// identical terminal outcomes, stage dispatch counts, and gate evaluation
// counts — the §3.3 conformance property at the outcome level.
func TestCrossRunnerTerminalOutcomeParity(t *testing.T) {
	reviewLoop := crSpec("implement",
		[]apiv1.Task{crTask("implement", "review")},
		[]apiv1.Gate{crGate("review", map[string]string{"pass": wf.TerminalComplete, "fail": "implement"})})
	reviewAbort := crSpec("implement",
		[]apiv1.Task{crTask("implement", "review")},
		[]apiv1.Gate{crGate("review", map[string]string{"pass": wf.TerminalComplete, "fail": wf.TargetAbort})})

	cases := []struct {
		name        string
		spec        apiv1.WorkflowSpec
		results     map[string][]apiv1.ResultEnvelope
		maxRepasses int
		wantStatus  string
		wantCalls   map[string]int
		wantEvals   int
		wantCode    string
	}{
		{
			name:       "success passes the gate and completes",
			spec:       reviewAbort,
			results:    nil,
			wantStatus: StatusCompleted,
			wantCalls:  map[string]int{"implement": 1},
			wantEvals:  1,
		},
		{
			name:       "gate fail branch aborts",
			spec:       reviewAbort,
			results:    map[string][]apiv1.ResultEnvelope{"implement": {failureResult("build_failed", "nope")}},
			wantStatus: StatusBlocked,
			wantCalls:  map[string]int{"implement": 1},
			wantEvals:  1,
		},
		{
			name: "repass then pass completes",
			spec: reviewLoop,
			results: map[string][]apiv1.ResultEnvelope{"implement": {
				failureResult("x", "first try fails"),
				{Status: apiv1.ResultSuccess},
			}},
			maxRepasses: 3,
			wantStatus:  StatusCompleted,
			wantCalls:   map[string]int{"implement": 2},
			wantEvals:   2,
		},
		{
			name:        "repass budget exhaustion escalates",
			spec:        reviewLoop,
			results:     map[string][]apiv1.ResultEnvelope{"implement": {failureResult("x", "always fails")}},
			maxRepasses: 1,
			wantStatus:  StatusEscalated,
			wantCalls:   map[string]int{"implement": 2},
			wantEvals:   2,
		},
		{
			name: "escalation routes through the escalate control branch",
			spec: crSpec("implement",
				[]apiv1.Task{crTask("implement", "review"), crTask("park-escalated", wf.TargetEscalate)},
				[]apiv1.Gate{crGate("review", map[string]string{
					"pass": wf.TerminalComplete, "fail": "implement", wf.BranchEscalate: "park-escalated",
				})}),
			results:     map[string][]apiv1.ResultEnvelope{"implement": {failureResult("x", "always fails")}},
			maxRepasses: 1,
			wantStatus:  StatusEscalated,
			wantCalls:   map[string]int{"implement": 2, "park-escalated": 1},
			wantEvals:   2,
		},
		{
			name:       "blocked halts at the escalated terminal",
			spec:       reviewAbort,
			results:    map[string][]apiv1.ResultEnvelope{"implement": {{Status: apiv1.ResultBlocked, Summary: "needs a human"}}},
			wantStatus: StatusEscalated,
			wantCalls:  map[string]int{"implement": 1},
			wantEvals:  0,
		},
		{
			name:       "no-work short-circuits to completed",
			spec:       reviewAbort,
			results:    map[string][]apiv1.ResultEnvelope{"implement": {{Status: apiv1.ResultNoWork, Summary: "empty tick"}}},
			wantStatus: StatusCompleted,
			wantCalls:  map[string]int{"implement": 1},
			wantEvals:  0,
		},
		{
			name:       "failure on a non-gate terminal stage fails the run",
			spec:       crSpec("implement", []apiv1.Task{crTask("implement", "")}, nil),
			results:    map[string][]apiv1.ResultEnvelope{"implement": {failureResult("boom_code", "it broke")}},
			wantStatus: StatusFailed,
			wantCalls:  map[string]int{"implement": 1},
			wantEvals:  0,
			wantCode:   "boom_code",
		},
		{
			name:       "reserved task next @abort",
			spec:       crSpec("implement", []apiv1.Task{crTask("implement", wf.TargetAbort)}, nil),
			wantStatus: StatusBlocked,
			wantCalls:  map[string]int{"implement": 1},
			wantEvals:  0,
		},
		{
			name:       "reserved task next @escalate",
			spec:       crSpec("implement", []apiv1.Task{crTask("implement", wf.TargetEscalate)}, nil),
			wantStatus: StatusEscalated,
			wantCalls:  map[string]int{"implement": 1},
			wantEvals:  0,
		},
		{
			name: "non-pass gate cannot hide an unresolved failure",
			spec: crSpec("implement",
				[]apiv1.Task{crTask("implement", "review")},
				[]apiv1.Gate{crGate("review", map[string]string{"pass": wf.TerminalComplete, "fail": wf.TerminalComplete})}),
			results:    map[string][]apiv1.ResultEnvelope{"implement": {failureResult("hidden", "swept under")}},
			wantStatus: StatusFailed,
			wantCalls:  map[string]int{"implement": 1},
			wantEvals:  1,
			wantCode:   "hidden",
		},
		{
			name: "tolerated failure advances",
			spec: crSpec("implement", []apiv1.Task{
				func() apiv1.Task {
					task := crTask("implement", "deploy")
					task.ContinueOnError = true
					return task
				}(),
				crTask("deploy", ""),
			}, nil),
			results:    map[string][]apiv1.ResultEnvelope{"implement": {failureResult("soft", "tolerated")}},
			wantStatus: StatusCompleted,
			wantCalls:  map[string]int{"implement": 1, "deploy": 1},
			wantEvals:  0,
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			machine, err := wf.Compile(
				wf.Definition{Name: "cross", Version: 1, Spec: tc.spec},
				wf.WithPreviewFeatures(true),
			)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			runID := fmt.Sprintf("run-cross-%02d", i)

			// Local runner walk.
			localStub := &scriptedStages{results: tc.results}
			localAuto := &countingAutomated{inner: gate.NewAutomatedEvaluator()}
			instanceRoot := t.TempDir()
			wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
			if err != nil {
				t.Fatalf("new worktree manager: %v", err)
			}
			r, err := runner.New(runner.Config{
				NewDeterministic: func(runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Deterministic, error) {
					return localStub, nil
				},
				Automated:   localAuto,
				MaxRepasses: tc.maxRepasses,
				Worktrees:   wtMgr,
				RunsDir:     filepath.Join(instanceRoot, "runs"),
				ScratchDir:  filepath.Join(instanceRoot, "scratch"),
			})
			if err != nil {
				t.Fatalf("runner.New: %v", err)
			}
			localRes, err := r.Start(context.Background(), runner.StartInput{
				RunID:   runID,
				Machine: machine,
				Gaggle:  "web",
				Trigger: journal.Trigger{Kind: journal.TriggerManual},
				RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
			})
			if err != nil {
				t.Fatalf("local Start: %v", err)
			}

			// Engine walk of the same definition, fixtures, and evaluator.
			engineStub := &scriptedStages{results: tc.results}
			engineAuto := &countingAutomated{inner: gate.NewAutomatedEvaluator()}
			in := RunInput{
				RunID:                  runID,
				Gaggle:                 "web",
				WorkflowName:           "cross",
				Version:                1,
				PreviewFeaturesEnabled: boolPointer(true),
				Spec:                   tc.spec,
				RepoRef:                apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
				MaxRepasses:            tc.maxRepasses,
			}
			var ts testsuite.WorkflowTestSuite
			env := ts.NewTestWorkflowEnvironment()
			env.RegisterActivity(&Activities{Det: engineStub, Auto: engineAuto, Workspaces: testWorkspaces(t)})
			env.ExecuteWorkflow(Run, in)
			if err := env.GetWorkflowError(); err != nil {
				t.Fatalf("engine workflow error: %v", err)
			}
			var engineRes RunResult
			if err := env.GetWorkflowResult(&engineRes); err != nil {
				t.Fatalf("engine result: %v", err)
			}

			// Terminal outcome parity (§3.3): same fixture, same terminal.
			localStatus := statusForPhase(t, localRes.Phase)
			if engineRes.Status != tc.wantStatus || localStatus != tc.wantStatus {
				t.Errorf("terminal outcomes: engine=%q local=%q, want both %q", engineRes.Status, localStatus, tc.wantStatus)
			}
			if tc.wantCode != "" {
				if engineRes.FailureCode != tc.wantCode || localRes.FailureCode != tc.wantCode {
					t.Errorf("failure codes: engine=%q local=%q, want both %q", engineRes.FailureCode, localRes.FailureCode, tc.wantCode)
				}
			}
			for stage, want := range tc.wantCalls {
				if got := engineStub.callCounts()[stage]; got != want {
					t.Errorf("engine dispatched %q %d times, want %d", stage, got, want)
				}
				if got := localStub.callCounts()[stage]; got != want {
					t.Errorf("local runner dispatched %q %d times, want %d", stage, got, want)
				}
			}
			if got := engineAuto.count(); got != tc.wantEvals {
				t.Errorf("engine gate evaluations = %d, want %d", got, tc.wantEvals)
			}
			if got := localAuto.count(); got != tc.wantEvals {
				t.Errorf("local gate evaluations = %d, want %d", got, tc.wantEvals)
			}
		})
	}
}

// TestResolveGateOutcome pins the workflow-side port of gate.Evaluator's
// repass tracking: pass resets, non-pass increments, exceeding the budget
// escalates via the escalate control branch (or @escalate without one), and
// an unmapped outcome is an error, never a silent pass.
func TestResolveGateOutcome(t *testing.T) {
	branches := map[string]string{"pass": wf.TerminalComplete, "fail": "implement"}
	g := crGate("review", branches)
	withEscalate := crGate("review", map[string]string{
		"pass": wf.TerminalComplete, "fail": "implement", wf.BranchEscalate: "park",
	})

	t.Run("pass resets the budget", func(t *testing.T) {
		attempts := map[string]int{"review": 2}
		gr, err := resolveGateOutcome(g, gate.OutcomePass, attempts, 3)
		if err != nil {
			t.Fatalf("resolveGateOutcome: %v", err)
		}
		if gr.Escalated || gr.Attempt != 0 || attempts["review"] != 0 || gr.Target != wf.TerminalComplete {
			t.Fatalf("pass result = %+v attempts=%d, want reset to 0 and the pass branch", gr, attempts["review"])
		}
	})

	t.Run("non-pass within budget follows the gate's own branch", func(t *testing.T) {
		attempts := map[string]int{}
		gr, err := resolveGateOutcome(g, gate.OutcomeFail, attempts, 1)
		if err != nil {
			t.Fatalf("resolveGateOutcome: %v", err)
		}
		if gr.Escalated || gr.Attempt != 1 || gr.Target != "implement" {
			t.Fatalf("result = %+v, want attempt 1 routed to implement", gr)
		}
	})

	t.Run("exhaustion escalates to @escalate without a control branch", func(t *testing.T) {
		attempts := map[string]int{"review": 1}
		gr, err := resolveGateOutcome(g, gate.OutcomeFail, attempts, 1)
		if err != nil {
			t.Fatalf("resolveGateOutcome: %v", err)
		}
		if !gr.Escalated || gr.Target != wf.TargetEscalate {
			t.Fatalf("result = %+v, want escalation to @escalate", gr)
		}
	})

	t.Run("exhaustion routes through the escalate control branch", func(t *testing.T) {
		attempts := map[string]int{"review": 1}
		gr, err := resolveGateOutcome(withEscalate, gate.OutcomeFail, attempts, 1)
		if err != nil {
			t.Fatalf("resolveGateOutcome: %v", err)
		}
		if !gr.Escalated || gr.Target != "park" {
			t.Fatalf("result = %+v, want escalation routed to park", gr)
		}
	})

	t.Run("unmapped outcome errors", func(t *testing.T) {
		if _, err := resolveGateOutcome(g, "maybe", map[string]int{}, 1); err == nil || !strings.Contains(err.Error(), "GT-002") {
			t.Fatalf("err = %v, want the GT-002 no-silent-pass error", err)
		}
	})
}

// TestGateEvaluatorInfraRetry mirrors internal/gate's #765 semantics on the
// engine: a gate's declared retry bound retries transient
// (infrastructure-marked) evaluator failures only; a non-transient error
// fails immediately regardless of the bound.
func TestGateEvaluatorInfraRetry(t *testing.T) {
	spec := crSpec("implement",
		[]apiv1.Task{crTask("implement", "review")},
		[]apiv1.Gate{{
			Name: "review", Evaluator: apiv1.EvaluatorAutomated,
			Automated: &apiv1.AutomatedGate{Check: "status-equals", Retry: &apiv1.RetryPolicy{MaxAttempts: 3, BackoffSeconds: 2}},
			Branches:  map[string]string{"pass": wf.TerminalComplete, "fail": wf.TargetAbort},
		}})

	t.Run("transient evaluator failure retries within the bound", func(t *testing.T) {
		calls := 0
		auto := automatedFunc(func(_ context.Context, _ apiv1.AutomatedGate, _ apiv1.InvocationEnvelope) (string, error) {
			calls++
			if calls == 1 {
				return "", invoke.InfrastructureFailure(errors.New("evaluator worker lost"))
			}
			return gate.OutcomePass, nil
		})
		var ts testsuite.WorkflowTestSuite
		env := ts.NewTestWorkflowEnvironment()
		env.RegisterActivity(&Activities{Det: &scriptedStages{}, Auto: auto, Workspaces: testWorkspaces(t)})
		env.ExecuteWorkflow(Run, runInput("gate-retry", spec))
		if err := env.GetWorkflowError(); err != nil {
			t.Fatalf("workflow error: %v", err)
		}
		if calls != 2 {
			t.Fatalf("evaluator calls = %d, want 2 (one transient failure, one success)", calls)
		}
	})

	t.Run("non-transient evaluator failure fails fast", func(t *testing.T) {
		calls := 0
		auto := automatedFunc(func(_ context.Context, _ apiv1.AutomatedGate, _ apiv1.InvocationEnvelope) (string, error) {
			calls++
			return "", errors.New("misconfigured check")
		})
		var ts testsuite.WorkflowTestSuite
		env := ts.NewTestWorkflowEnvironment()
		env.RegisterActivity(&Activities{Det: &scriptedStages{}, Auto: auto, Workspaces: testWorkspaces(t)})
		env.ExecuteWorkflow(Run, runInput("gate-retry-fatal", spec))
		err := env.GetWorkflowError()
		if err == nil || !strings.Contains(err.Error(), "misconfigured check") {
			t.Fatalf("workflow error = %v, want the evaluator's own failure", err)
		}
		if calls != 1 {
			t.Fatalf("evaluator calls = %d, want 1 (no wasted retries on a non-transient error)", calls)
		}
	})
}

// TestMaxStepsSharedWithLocalRunner locks the shared-ceiling requirement
// (#624): the engine's step guard IS the local runner's, not a copied
// literal that can drift.
func TestMaxStepsSharedWithLocalRunner(t *testing.T) {
	if maxSteps != runner.DefaultMaxSteps {
		t.Fatalf("engine maxSteps = %d, want runner.DefaultMaxSteps (%d)", maxSteps, runner.DefaultMaxSteps)
	}
}
