package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

type capturingFanInExecutor struct {
	delegate *stubDeterministic
	envs     map[string]apiv1.InvocationEnvelope
}

func (c *capturingFanInExecutor) Run(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	c.envs[env.TaskID] = env
	return c.delegate.Run(ctx, env, run)
}

type emptyRepassFanInExecutor struct {
	calls map[string]int
}

func (e *emptyRepassFanInExecutor) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	e.calls[env.TaskID]++
	switch {
	case strings.HasSuffix(env.TaskID, ":review-security"):
		if e.calls[env.TaskID] == 1 {
			return apiv1.ResultEnvelope{
				Status:  apiv1.ResultSuccess,
				Outputs: map[string]any{"summary": "superseded"},
			}, nil
		}
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	case strings.HasSuffix(env.TaskID, ":review-performance"):
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	case strings.HasSuffix(env.TaskID, ":collate"):
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	default:
		return apiv1.ResultEnvelope{}, fmt.Errorf("unexpected task %q", env.TaskID)
	}
}

type failThenPassAutomated struct {
	calls int
}

func (a *failThenPassAutomated) Evaluate(context.Context, apiv1.AutomatedGate, apiv1.InvocationEnvelope) (string, error) {
	a.calls++
	if a.calls == 1 {
		return gate.OutcomeFail, nil
	}
	return gate.OutcomePass, nil
}

func fanInMachine(t *testing.T, failedPerformance bool) *workflow.Machine {
	t.Helper()
	branchTask := func(name string, continueOnError bool) apiv1.Task {
		return apiv1.Task{
			Name: name, Type: apiv1.TaskDeterministic, Goal: name,
			Run:             &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
			ExpectedOutputs: []string{"summary"},
			ContinueOnError: continueOnError,
			Next:            workflow.TargetJoin,
		}
	}
	tasks := []apiv1.Task{
		branchTask("review-security", false),
		branchTask("review-performance", false),
	}
	branches := []apiv1.Branch{
		{Name: "security", Start: "review-security"},
		{Name: "performance", Start: "review-performance"},
	}
	inputsFrom := map[string]string{
		"security":    "fan.security.review-security.summary",
		"performance": "fan.performance.summary",
	}
	if failedPerformance {
		inputsFrom[BranchCompletenessInput] = "fan.performance.summary"
	} else {
		inputsFrom[BranchCompletenessInput] = "fan.security.summary"
		tasks = append(tasks, branchTask("review-reliability", false))
		branches = append(branches, apiv1.Branch{Name: "reliability", Start: "review-reliability"})
		inputsFrom["reliability"] = "fan.reliability.review-reliability.summary"
	}
	tasks = append(tasks, apiv1.Task{
		Name: "collate", Type: apiv1.TaskDeterministic, Goal: "collate reports",
		Run:        &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
		InputsFrom: inputsFrom,
		Next:       workflow.TerminalComplete,
	})
	machine, err := workflow.Compile(workflow.Definition{
		Name: "fan-in", Version: 1, DSLVersion: "2.0",
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "goobers",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:    "fan",
			Tasks:    tasks,
			Parallels: []apiv1.Parallel{{
				Name: "fan", FailurePolicy: apiv1.BranchContinueOnError,
				Branches: branches, Join: "collate",
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile fan-in machine: %v", err)
	}
	return machine
}

func branchGateFanInMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	task := func(name, next string) apiv1.Task {
		return apiv1.Task{
			Name: name, Type: apiv1.TaskDeterministic, Goal: name,
			Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
			Next: next,
		}
	}
	machine, err := workflow.Compile(workflow.Definition{
		Name: "branch-gate-fan-in", Version: 1, DSLVersion: "2.0",
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "goobers",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:    "fan",
			Tasks: []apiv1.Task{
				task("review-security", "accept-security"),
				task("review-performance", workflow.TargetJoin),
				task("collate", workflow.TerminalComplete),
				task("recover", workflow.TerminalComplete),
			},
			Gates: []apiv1.Gate{{
				Name:      "accept-security",
				Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "status-equals"},
				Branches: map[string]string{
					gate.OutcomePass: workflow.TargetJoin,
					gate.OutcomeFail: workflow.TargetJoin,
				},
			}},
			Parallels: []apiv1.Parallel{{
				Name: "fan", FailurePolicy: apiv1.BranchAllOrNothing, OnFailure: "recover",
				Branches: []apiv1.Branch{
					{Name: "security", Start: "review-security"},
					{Name: "performance", Start: "review-performance"},
				},
				Join: "collate",
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile branch gate fan-in machine: %v", err)
	}
	return machine
}

func emptyRepassFanInMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	task := func(name, next string) apiv1.Task {
		return apiv1.Task{
			Name: name, Type: apiv1.TaskDeterministic, Goal: name,
			Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
			Next: next,
		}
	}
	machine, err := workflow.Compile(workflow.Definition{
		Name: "empty-repass-fan-in", Version: 1, DSLVersion: "2.0",
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "goobers",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:    "fan",
			Tasks: []apiv1.Task{
				task("review-security", "accept-security"),
				task("review-performance", workflow.TargetJoin),
				{
					Name: "collate", Type: apiv1.TaskDeterministic, Goal: "collate",
					Run:        &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
					InputsFrom: map[string]string{"security": "fan.security.review-security.summary"},
					Next:       workflow.TerminalComplete,
				},
			},
			Gates: []apiv1.Gate{{
				Name:      "accept-security",
				Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "status-equals"},
				Branches: map[string]string{
					gate.OutcomePass: workflow.TargetJoin,
					gate.OutcomeFail: "review-security",
				},
			}},
			Parallels: []apiv1.Parallel{{
				Name: "fan", FailurePolicy: apiv1.BranchContinueOnError,
				Branches: []apiv1.Branch{
					{Name: "security", Start: "review-security"},
					{Name: "performance", Start: "review-performance"},
				},
				Join: "collate",
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile empty repass fan-in machine: %v", err)
	}
	return machine
}

func continueOnErrorFanInMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	task := func(name string, continueOnError bool, next string) apiv1.Task {
		return apiv1.Task{
			Name: name, Type: apiv1.TaskDeterministic, Goal: name,
			Run:             &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
			ContinueOnError: continueOnError,
			Next:            next,
		}
	}
	machine, err := workflow.Compile(workflow.Definition{
		Name: "continue-on-error-fan-in", Version: 1, DSLVersion: "2.0",
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "goobers",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:    "fan",
			Tasks: []apiv1.Task{
				task("review-security", true, workflow.TargetJoin),
				task("review-performance", false, workflow.TargetJoin),
				task("collate", false, workflow.TerminalComplete),
			},
			Parallels: []apiv1.Parallel{{
				Name: "fan", FailurePolicy: apiv1.BranchContinueOnError,
				Branches: []apiv1.Branch{
					{Name: "security", Start: "review-security"},
					{Name: "performance", Start: "review-performance"},
				},
				Join: "collate",
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile continue-on-error fan-in machine: %v", err)
	}
	return machine
}

func agenticGateFanInMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	task := func(name string) apiv1.Task {
		return apiv1.Task{
			Name: name, Type: apiv1.TaskDeterministic, Goal: name,
			Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
			Next: workflow.TargetJoin,
		}
	}
	machine, err := workflow.Compile(workflow.Definition{
		Name: "agentic-gate-fan-in", Version: 1, DSLVersion: "2.0",
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "goobers",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:    "fan",
			Tasks: []apiv1.Task{
				task("review-security"),
				task("review-performance"),
			},
			Gates: []apiv1.Gate{{
				Name:      "collate",
				Evaluator: apiv1.EvaluatorAgentic,
				Agentic:   &apiv1.AgenticGate{Goober: "reviewer"},
				Branches: map[string]string{
					string(apiv1.VerdictPass):         workflow.TerminalComplete,
					string(apiv1.VerdictNeedsChanges): workflow.TargetEscalate,
					string(apiv1.VerdictFail):         workflow.TargetAbort,
				},
			}},
			Parallels: []apiv1.Parallel{{
				Name: "fan", FailurePolicy: apiv1.BranchContinueOnError,
				Branches: []apiv1.Branch{
					{Name: "security", Start: "review-security"},
					{Name: "performance", Start: "review-performance"},
				},
				Join: "collate",
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile agentic gate fan-in machine: %v", err)
	}
	return machine
}

func skippedJoinMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	task := func(name, next string) apiv1.Task {
		return apiv1.Task{
			Name: name, Type: apiv1.TaskDeterministic, Goal: name,
			Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
			Next: next,
		}
	}
	machine, err := workflow.Compile(workflow.Definition{
		Name: "skipped-join", Version: 1, DSLVersion: "2.0",
		Spec: apiv1.WorkflowSpec{
			Gaggle: "goobers", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}}, Start: "prepare",
			Tasks: []apiv1.Task{
				task("prepare", "fan"),
				task("review-security", workflow.TargetJoin),
				task("review-performance", workflow.TargetJoin),
				task("collate", workflow.TerminalComplete),
				task("recover", workflow.TerminalComplete),
			},
			Parallels: []apiv1.Parallel{{
				Name: "fan", FailurePolicy: apiv1.BranchFailFast, Join: "collate", OnFailure: "recover",
				Branches: []apiv1.Branch{
					{Name: "security", Start: "review-security"},
					{Name: "performance", Start: "review-performance"},
				},
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile skipped-join machine: %v", err)
	}
	return machine
}

func TestRunnerFansInThreeBranchScalarsAndArtifacts(t *testing.T) {
	envs := map[string]apiv1.InvocationEnvelope{}
	results := map[string]stubTaskResult{}
	for _, runID := range []string{"fan-in-a", "fan-in-b"} {
		for _, branch := range []string{"security", "performance", "reliability"} {
			results[runID+":review-"+branch] = stubTaskResult{
				status: apiv1.ResultSuccess, outputs: map[string]interface{}{"summary": branch},
				artifactName: branch + ".md", artifactData: []byte(branch), artifactMediaType: "text/markdown",
			}
		}
		results[runID+":collate"] = stubTaskResult{status: apiv1.ResultSuccess}
	}
	r, _ := newTestRunnerWithDeterministic(t, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		return &capturingFanInExecutor{
			delegate: &stubDeterministic{rec: rec, byTask: results},
			envs:     envs,
		}, nil
	}, nil)
	r.cfg.ScratchDir = t.TempDir()
	spans := &fakeSpanStarter{}
	r.cfg.Telemetry = spans

	for _, runID := range []string{"fan-in-a", "fan-in-b"} {
		result, err := r.Start(context.Background(), StartInput{
			RunID: runID, Machine: fanInMachine(t, false), Gaggle: "goobers",
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
			RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		})
		if err != nil {
			t.Fatalf("%s: Start: %v", runID, err)
		}
		if result.Phase != journal.PhaseCompleted {
			t.Fatalf("%s: phase = %q, want completed", runID, result.Phase)
		}
	}

	first := envs["fan-in-a:collate"]
	for _, branch := range []string{"security", "performance", "reliability"} {
		if got := first.Inputs[branch]; got != branch {
			t.Errorf("join input %q = %#v, want %q", branch, got, branch)
		}
	}
	if len(first.ContextPointers) != 3 {
		t.Fatalf("join pointers = %+v, want one artifact from each branch", first.ContextPointers)
	}
	for i, branch := range []string{"security", "performance", "reliability"} {
		pointer := first.ContextPointers[i]
		if pointer.Branch != i+1 || pointer.BranchName != branch || pointer.Artifact == nil {
			t.Errorf("pointer %d = %+v, want artifact tagged branch %d/%q", i, pointer, i+1, branch)
		}
	}
	completeness, ok := first.Inputs[BranchCompletenessInput].([]journal.BranchOutcome)
	if !ok || len(completeness) != 3 {
		t.Fatalf("branch completeness = %#v, want three outcomes", first.Inputs[BranchCompletenessInput])
	}
	for i, branch := range completeness {
		if branch.Branch != i+1 || branch.Status != journal.BranchSucceeded || branch.Artifacts != 1 {
			t.Errorf("completeness %d = %+v, want succeeded branch with one artifact", i, branch)
		}
	}

	firstPointers, err := json.Marshal(first.ContextPointers)
	if err != nil {
		t.Fatal(err)
	}
	secondPointers, err := json.Marshal(envs["fan-in-b:collate"].ContextPointers)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstPointers) != string(secondPointers) {
		t.Errorf("journaled pointer sets differ across runs:\n%s\n%s", firstPointers, secondPointers)
	}
	wantBranches := []int{1, 2, 3, 0, 1, 2, 3, 0}
	if len(spans.taskAttrs) != len(wantBranches) {
		t.Fatalf("task span attributes = %#v, want %d branch and join spans", spans.taskAttrs, len(wantBranches))
	}
	for i, want := range wantBranches {
		if got := spans.taskAttrs[i].Branch; got != want {
			t.Errorf("task span %d branch = %d, want %d", i, got, want)
		}
	}
}

func TestRunnerPassingBranchGateClearsTaskFailure(t *testing.T) {
	envs := map[string]apiv1.InvocationEnvelope{}
	results := map[string]stubTaskResult{
		"branch-gate:review-security": {
			status:    apiv1.ResultFailure,
			errorInfo: &apiv1.ErrorInfo{Code: "review_failed", Message: "review requires gate approval"},
		},
		"branch-gate:review-performance": {
			status:  apiv1.ResultSuccess,
			outputs: map[string]interface{}{"summary": "performance"},
		},
		"branch-gate:collate": {status: apiv1.ResultSuccess},
		"branch-gate:recover": {status: apiv1.ResultSuccess},
	}
	r, _ := newTestRunnerWithDeterministic(t, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		return &capturingFanInExecutor{
			delegate: &stubDeterministic{rec: rec, byTask: results},
			envs:     envs,
		}, nil
	}, fixedOutcomeAutomated(gate.OutcomePass))
	r.cfg.ScratchDir = t.TempDir()

	result, err := r.Start(context.Background(), StartInput{
		RunID: "branch-gate", Machine: branchGateFanInMachine(t), Gaggle: "goobers",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", result.Phase)
	}
	join, ok := envs["branch-gate:collate"]
	if !ok {
		t.Fatalf("join was suppressed; invocations = %+v", envs)
	}
	if _, recovered := envs["branch-gate:recover"]; recovered {
		t.Fatalf("passing branch gate incorrectly routed through recovery")
	}
	completeness, ok := join.Inputs[BranchCompletenessInput].([]journal.BranchOutcome)
	if !ok || len(completeness) != 2 || completeness[0].Status != journal.BranchNoOutput {
		t.Fatalf("branch completeness = %#v, want cleared security failure", join.Inputs[BranchCompletenessInput])
	}
}

func TestGateClearsFailure(t *testing.T) {
	tests := []struct {
		name string
		gate apiv1.Gate
		gr   gate.Result
		want bool
	}{
		{name: "passing automated gate", gate: apiv1.Gate{Evaluator: apiv1.EvaluatorAutomated}, gr: gate.Result{Outcome: gate.OutcomePass}, want: true},
		{name: "human decision", gate: apiv1.Gate{Evaluator: apiv1.EvaluatorHuman}, gr: gate.Result{Outcome: gate.OutcomeFail}, want: true},
		{name: "failing automated gate", gate: apiv1.Gate{Evaluator: apiv1.EvaluatorAutomated}, gr: gate.Result{Outcome: gate.OutcomeFail}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := gateClearsFailure(test.gr, test.gate); got != test.want {
				t.Fatalf("gateClearsFailure() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestRunnerAgenticJoinReceivesBranchArtifactsOnce(t *testing.T) {
	const runID = "agentic-gate-fan-in"
	reviewer := &capturingReviewer{}
	results := map[string]stubTaskResult{
		runID + ":review-security": {
			status: apiv1.ResultSuccess, artifactName: "security.md",
			artifactData: []byte("security"), artifactMediaType: "text/markdown",
		},
		runID + ":review-performance": {
			status: apiv1.ResultSuccess, artifactName: "performance.md",
			artifactData: []byte("performance"), artifactMediaType: "text/markdown",
		},
	}
	r, _ := newRerunTestRunner(t, func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
		return reviewer, nil
	}, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		return &stubDeterministic{rec: rec, byTask: results}, nil
	})
	r.cfg.ScratchDir = t.TempDir()

	result, err := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: agenticGateFanInMachine(t), Gaggle: "goobers",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", result.Phase)
	}
	if !reviewer.called {
		t.Fatal("agentic join reviewer was not invoked")
	}
	// Exactly the two branch artifacts, each tagged with its own branch — no
	// diff pointer, since rule 9 now forbids a writable repo workspace inside
	// any branch (any width), so a branch can never produce a committed diff
	// for the join to see.
	if len(reviewer.gotPointers) != 2 {
		t.Fatalf("reviewer pointers = %+v, want exactly two branch artifacts", reviewer.gotPointers)
	}
	for i, branch := range []string{"security", "performance"} {
		pointer := reviewer.gotPointers[i]
		if pointer.Branch != i+1 || pointer.BranchName != branch || pointer.Artifact == nil {
			t.Errorf("pointer %d = %+v, want artifact tagged branch %d/%q", i, pointer, i+1, branch)
		}
	}
}

func TestRunnerOmitsFailedBranchInputAndReportsCompleteness(t *testing.T) {
	envs := map[string]apiv1.InvocationEnvelope{}
	results := map[string]stubTaskResult{
		"fan-in-failed:review-security": {
			status: apiv1.ResultSuccess, outputs: map[string]interface{}{"summary": "security"},
		},
		"fan-in-failed:review-performance": {
			status:    apiv1.ResultFailure,
			errorInfo: &apiv1.ErrorInfo{Code: "review_failed", Message: "performance review failed"},
		},
		"fan-in-failed:collate": {status: apiv1.ResultSuccess},
	}
	r, _ := newTestRunnerWithDeterministic(t, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		return &capturingFanInExecutor{
			delegate: &stubDeterministic{rec: rec, byTask: results},
			envs:     envs,
		}, nil
	}, nil)
	r.cfg.ScratchDir = t.TempDir()

	_, err := r.Start(context.Background(), StartInput{
		RunID: "fan-in-failed", Machine: fanInMachine(t, true), Gaggle: "goobers",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	join := envs["fan-in-failed:collate"]
	if _, ok := join.Inputs["performance"]; ok {
		t.Errorf("failed branch input should be absent, got %#v", join.Inputs["performance"])
	}
	if got := join.Inputs["security"]; got != "security" {
		t.Errorf("successful branch input = %#v, want security", got)
	}
	completeness, ok := join.Inputs[BranchCompletenessInput].([]journal.BranchOutcome)
	if !ok || len(completeness) != 2 || completeness[1].Status != journal.BranchFailed {
		t.Errorf("branch completeness = %#v, want failed performance branch", join.Inputs[BranchCompletenessInput])
	}
}

func TestRunnerResumeAfterSkippedJoinKeepsOnlyRootPointers(t *testing.T) {
	machine := skippedJoinMachine(t)
	capturing := &capturingDeterministic{}
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return capturing, nil
	}, nil)
	r.cfg.ScratchDir = t.TempDir()

	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "fan-in-skipped-resume", Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "goobers", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	jr.SetMachineState("recover")
	rootRef, err := jr.RecordArtifact("prepare.md", []byte("prepare"))
	if err != nil {
		t.Fatalf("record root artifact: %v", err)
	}
	branchRef, err := jr.RecordArtifact("security.md", []byte("security"))
	if err != nil {
		t.Fatalf("record branch artifact: %v", err)
	}
	events := []journal.Event{
		{Type: journal.EventStageStarted, Stage: "prepare", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "prepare", Attempt: 1, Status: string(apiv1.ResultSuccess),
			Artifacts: []journal.Ref{{Path: rootRef.Path, Digest: rootRef.Digest, Size: rootRef.Size}}},
		{Type: journal.EventParallelStarted, Parallel: "fan", Completeness: []journal.BranchOutcome{
			{Branch: 1, Name: "security"}, {Branch: 2, Name: "performance"},
		}},
		{Type: journal.EventBranchStarted, Parallel: "fan", Branch: 1, BranchName: "security", Stage: "review-security"},
		{Type: journal.EventStageStarted, Stage: "review-security", Attempt: 1, Branch: 1},
		{Type: journal.EventStageFinished, Stage: "review-security", Attempt: 1, Branch: 1, Status: string(apiv1.ResultFailure),
			Artifacts: []journal.Ref{{Path: branchRef.Path, Digest: branchRef.Digest, Size: branchRef.Size}}},
		{Type: journal.EventBranchFinished, Parallel: "fan", Branch: 1, BranchName: "security", BranchStatus: journal.BranchFailed},
		{Type: journal.EventBranchFinished, Parallel: "fan", Branch: 2, BranchName: "performance", BranchStatus: journal.BranchCancelled},
		{Type: journal.EventParallelFinished, Parallel: "fan", Target: "recover", Completeness: []journal.BranchOutcome{
			{Branch: 1, Name: "security", Status: journal.BranchFailed, Artifacts: 1},
			{Branch: 2, Name: "performance", Status: journal.BranchCancelled},
		}},
	}
	for _, event := range events {
		if err := jr.Append(event); err != nil {
			t.Fatalf("append %s: %v", event.Type, err)
		}
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}

	result, err := r.Resume(context.Background(), ResumeInput{
		RunID: "fan-in-skipped-resume", Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", result.Phase)
	}
	if capturing.calls != 1 {
		t.Fatalf("onFailure calls = %d, want 1", capturing.calls)
	}
	if len(capturing.lastEnv.ContextPointers) != 1 {
		t.Fatalf("onFailure pointers = %+v, want only the root pointer", capturing.lastEnv.ContextPointers)
	}
	pointer := capturing.lastEnv.ContextPointers[0]
	if pointer.Name != "prepare.artifact[0]" || pointer.Artifact == nil || pointer.Artifact.Digest != rootRef.Digest {
		t.Fatalf("onFailure pointer = %+v, want the root prepare artifact", pointer)
	}
}

func TestRunnerResumeAfterParallelFinishedUsesJoinTarget(t *testing.T) {
	const runID = "fan-in-finished-resume"
	machine := fanInMachine(t, false)
	capturing := &capturingDeterministic{}
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return capturing, nil
	}, nil)
	r.cfg.ScratchDir = t.TempDir()

	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "goobers", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	jr.SetMachineState("review-reliability")
	branches := []struct {
		id    int
		name  string
		stage string
	}{
		{id: 1, name: "security", stage: "review-security"},
		{id: 2, name: "performance", stage: "review-performance"},
		{id: 3, name: "reliability", stage: "review-reliability"},
	}
	completeness := make([]journal.BranchOutcome, 0, len(branches))
	if err := jr.Append(journal.Event{
		Type: journal.EventParallelStarted, Parallel: "fan",
		Completeness: []journal.BranchOutcome{
			{Branch: 1, Name: "security"},
			{Branch: 2, Name: "performance"},
			{Branch: 3, Name: "reliability"},
		},
	}); err != nil {
		t.Fatalf("append parallel.started: %v", err)
	}
	for _, branch := range branches {
		events := []journal.Event{
			{Type: journal.EventBranchStarted, Parallel: "fan", Branch: branch.id, BranchName: branch.name, Stage: branch.stage},
			{Type: journal.EventStageStarted, Stage: branch.stage, Branch: branch.id, Attempt: 1},
			{Type: journal.EventStageFinished, Stage: branch.stage, Branch: branch.id, Attempt: 1,
				Status: string(apiv1.ResultSuccess), Outputs: map[string]any{"summary": branch.name}},
			{Type: journal.EventBranchFinished, Parallel: "fan", Branch: branch.id, BranchName: branch.name,
				BranchStatus: journal.BranchSucceeded},
		}
		for _, event := range events {
			if err := jr.Append(event); err != nil {
				t.Fatalf("append %s for %s: %v", event.Type, branch.name, err)
			}
		}
		completeness = append(completeness, journal.BranchOutcome{
			Branch: branch.id, Name: branch.name, Status: journal.BranchSucceeded,
		})
	}
	if err := jr.Append(journal.Event{
		Type: journal.EventParallelFinished, Parallel: "fan", Target: "collate", Completeness: completeness,
	}); err != nil {
		t.Fatalf("append parallel.finished: %v", err)
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}

	result, err := r.Resume(context.Background(), ResumeInput{
		RunID: runID, Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", result.Phase)
	}
	if capturing.calls != 1 || capturing.lastEnv.TaskID != runID+":collate" {
		t.Fatalf("resume invocation = %+v (calls %d), want only collate", capturing.lastEnv, capturing.calls)
	}
}

func TestRunnerResumeMidParallelContinuesRemainingBranches(t *testing.T) {
	const runID = "fan-in-mid-parallel-resume"
	machine := fanInMachine(t, false)
	envs := map[string]apiv1.InvocationEnvelope{}
	results := map[string]stubTaskResult{
		runID + ":review-reliability": {
			status: apiv1.ResultSuccess, outputs: map[string]interface{}{"summary": "reliability"},
		},
		runID + ":collate": {status: apiv1.ResultSuccess},
	}
	r, runsDir := newTestRunnerWithDeterministic(t, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		return &capturingFanInExecutor{
			delegate: &stubDeterministic{rec: rec, byTask: results},
			envs:     envs,
		}, nil
	}, nil)
	r.cfg.ScratchDir = t.TempDir()

	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "goobers", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	jr.SetMachineState("review-performance")
	events := []journal.Event{
		{Type: journal.EventParallelStarted, Parallel: "fan", Completeness: []journal.BranchOutcome{
			{Branch: 1, Name: "security"},
			{Branch: 2, Name: "performance"},
			{Branch: 3, Name: "reliability"},
		}},
		{Type: journal.EventBranchStarted, Parallel: "fan", Branch: 1, BranchName: "security", Stage: "review-security"},
		{Type: journal.EventStageStarted, Stage: "review-security", Branch: 1, Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "review-security", Branch: 1, Attempt: 1,
			Status: string(apiv1.ResultSuccess), Outputs: map[string]any{"summary": "security"}},
		{Type: journal.EventBranchFinished, Parallel: "fan", Branch: 1, BranchName: "security",
			BranchStatus: journal.BranchSucceeded},
		{Type: journal.EventBranchStarted, Parallel: "fan", Branch: 2, BranchName: "performance", Stage: "review-performance"},
		{Type: journal.EventStageStarted, Stage: "review-performance", Branch: 2, Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "review-performance", Branch: 2, Attempt: 1,
			Status: string(apiv1.ResultSuccess), Outputs: map[string]any{"summary": "performance"}},
	}
	for _, event := range events {
		if err := jr.Append(event); err != nil {
			t.Fatalf("append %s: %v", event.Type, err)
		}
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}

	result, err := r.Resume(context.Background(), ResumeInput{
		RunID: runID, Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", result.Phase)
	}
	if _, reran := envs[runID+":review-security"]; reran {
		t.Fatal("resume reran the settled security branch")
	}
	if _, reran := envs[runID+":review-performance"]; reran {
		t.Fatal("resume reran the already-finished performance stage")
	}
	join := envs[runID+":collate"]
	for _, branch := range []string{"security", "performance", "reliability"} {
		if got := join.Inputs[branch]; got != branch {
			t.Errorf("join input %q = %#v, want %q", branch, got, branch)
		}
	}
}

func TestRunnerBranchGateEmptyRepassClearsPriorOutputs(t *testing.T) {
	const runID = "fan-in-empty-repass"
	executor := &emptyRepassFanInExecutor{calls: map[string]int{}}
	automated := &failThenPassAutomated{}
	r, _ := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return executor, nil
	}, automated)
	r.cfg.ScratchDir = t.TempDir()

	result, err := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: emptyRepassFanInMachine(t), Gaggle: "goobers",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err == nil || !strings.Contains(err.Error(), `branch output "fan.security.review-security.summary" not found`) {
		t.Fatalf("Start error = %v, want missing latest-attempt branch output", err)
	}
	if result.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed", result.Phase)
	}
	if got := executor.calls[runID+":review-security"]; got != 2 {
		t.Fatalf("review-security calls = %d, want initial attempt and repass", got)
	}
	if got := executor.calls[runID+":collate"]; got != 0 {
		t.Fatalf("collate calls = %d, want stale output rejected before invocation", got)
	}
}

func TestRunnerResumeAfterParallelStartedRecordsFirstBranchStart(t *testing.T) {
	const runID = "fan-in-started-resume"
	machine := continueOnErrorFanInMachine(t)
	results := map[string]stubTaskResult{
		runID + ":review-security":    {status: apiv1.ResultSuccess},
		runID + ":review-performance": {status: apiv1.ResultSuccess},
		runID + ":collate":            {status: apiv1.ResultSuccess},
	}
	r, runsDir := newTestRunnerWithDeterministic(t, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		return &stubDeterministic{rec: rec, byTask: results}, nil
	}, nil)
	r.cfg.ScratchDir = t.TempDir()

	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "goobers", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	jr.SetMachineState("fan")
	if err := jr.Append(journal.Event{
		Type: journal.EventParallelStarted, Parallel: "fan", Completeness: []journal.BranchOutcome{
			{Branch: 1, Name: "security"},
			{Branch: 2, Name: "performance"},
		},
	}); err != nil {
		t.Fatalf("append parallel.started: %v", err)
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}

	result, err := r.Resume(context.Background(), ResumeInput{
		RunID: runID, Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", result.Phase)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	firstStarts := 0
	for _, event := range events {
		if event.Type == journal.EventBranchStarted && event.Branch == 1 && event.BranchName == "security" {
			firstStarts++
		}
	}
	if firstStarts != 1 {
		t.Fatalf("security branch.started count = %d, want 1", firstStarts)
	}
}

func TestRunnerResumeBranchTransitionKeepsBranchAttribution(t *testing.T) {
	const runID = "fan-in-branch-attribution-resume"
	machine := continueOnErrorFanInMachine(t)
	results := map[string]stubTaskResult{
		runID + ":review-performance": {status: apiv1.ResultSuccess},
		runID + ":collate":            {status: apiv1.ResultSuccess},
	}
	r, runsDir := newTestRunnerWithDeterministic(t, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		return &stubDeterministic{rec: rec, byTask: results}, nil
	}, nil)
	r.cfg.ScratchDir = t.TempDir()

	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "goobers", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	jr.SetMachineState("review-security")
	for _, event := range []journal.Event{
		{Type: journal.EventParallelStarted, Parallel: "fan", Completeness: []journal.BranchOutcome{
			{Branch: 1, Name: "security"},
			{Branch: 2, Name: "performance"},
		}},
		{Type: journal.EventBranchStarted, Parallel: "fan", Branch: 1, BranchName: "security", Stage: "review-security"},
		{Type: journal.EventStageStarted, Stage: "review-security", Branch: 1, Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "review-security", Branch: 1, Attempt: 1,
			Status: string(apiv1.ResultFailure), Outputs: map[string]any{"summary": "must-not-resume"}},
	} {
		if err := jr.Append(event); err != nil {
			t.Fatalf("append %s: %v", event.Type, err)
		}
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}

	result, err := r.Resume(context.Background(), ResumeInput{
		RunID: runID, Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", result.Phase)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	for _, event := range events {
		if event.Type == journal.EventError && event.Error != nil && event.Error.Code == toleratedFailureErrorCode {
			if event.Branch != 1 {
				t.Fatalf("tolerated failure branch = %d, want 1", event.Branch)
			}
			return
		}
	}
	t.Fatal("missing tolerated failure event")
}

func TestPendingParallelDoesNotCrossTerminalResume(t *testing.T) {
	machine := continueOnErrorFanInMachine(t)
	terminalResume := []journal.Event{
		{Type: journal.EventParallelStarted, Parallel: "fan"},
		{Type: journal.EventBranchStarted, Parallel: "fan", Branch: 1, BranchName: "security", Stage: "review-security"},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)},
		{Type: journal.EventRunResumed, Target: "collate"},
	}
	if par, _ := pendingParallel(terminalResume, machine); par != nil {
		t.Fatalf("pending parallel = %+v, want nil after terminal resume", par)
	}

	stageRerun := []journal.Event{
		{Type: journal.EventParallelStarted, Parallel: "fan"},
		{Type: journal.EventBranchStarted, Parallel: "fan", Branch: 1, BranchName: "security", Stage: "review-security"},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
		{Type: journal.EventStageRerunRequested, Stage: "review-security", Branch: 1},
	}
	if par, _ := pendingParallel(stageRerun, machine); par == nil {
		t.Fatal("pending parallel is nil after a stage rerun reopened the branch")
	}
}

// pendingParallel must reconstruct a branch's ORIGINAL start instant from its
// EventBranchStarted, not reset it to resumption time — a resumed branch gets
// its remaining branchTimeoutSeconds budget, not a fresh one (#1566).
func TestPendingParallelReconstructsBranchStartedAtForTimeoutBudget(t *testing.T) {
	machine := continueOnErrorFanInMachine(t)
	startedAt := time.Date(2026, time.July, 20, 9, 0, 0, 0, time.UTC)
	events := []journal.Event{
		{Type: journal.EventParallelStarted, Parallel: "fan"},
		{Type: journal.EventBranchStarted, Parallel: "fan", Branch: 1, BranchName: "security",
			Stage: "review-security", Time: startedAt},
	}
	par, _ := pendingParallel(events, machine)
	if par == nil {
		t.Fatal("pending parallel is nil")
	}
	branch := par.branch("security")
	if branch == nil {
		t.Fatal("reconstructed parallel has no security branch")
	}
	if !branch.startedAt.Equal(startedAt) {
		t.Fatalf("branch.startedAt = %v, want the original EventBranchStarted time %v", branch.startedAt, startedAt)
	}
}

func TestReconstructParallelPointersAndPendingFanIn(t *testing.T) {
	machine := fanInMachine(t, true)
	events := []journal.Event{
		{Type: journal.EventParallelStarted, Parallel: "fan", Completeness: []journal.BranchOutcome{
			{Branch: 1, Name: "security"}, {Branch: 2, Name: "performance"},
		}},
		{Type: journal.EventBranchStarted, Parallel: "fan", Branch: 2, BranchName: "performance"},
		{Type: journal.EventStageFinished, Stage: "review-performance", Branch: 2,
			Outputs:   map[string]any{"summary": "performance"},
			Artifacts: []journal.Ref{{Path: "performance.md", Digest: "sha256:performance"}}},
		{Type: journal.EventBranchStarted, Parallel: "fan", Branch: 1, BranchName: "security"},
		{Type: journal.EventStageFinished, Stage: "review-security", Branch: 1,
			Outputs:   map[string]any{"summary": "security"},
			Artifacts: []journal.Ref{{Path: "security.md", Digest: "sha256:security"}}},
		{Type: journal.EventParallelFinished, Parallel: "fan", Target: "collate", Completeness: []journal.BranchOutcome{
			{Branch: 1, Name: "security", Status: journal.BranchSucceeded, Artifacts: 1},
			{Branch: 2, Name: "performance", Status: journal.BranchFailed, Artifacts: 1},
		}},
	}

	pointers := reconstructPointers(events, machine)
	if len(pointers) != 2 || pointers[0].BranchName != "security" || pointers[1].BranchName != "performance" {
		t.Fatalf("reconstructed pointers = %+v, want declaration order with branch attribution", pointers)
	}
	fanIn := pendingFanIn(events, machine)
	if fanIn == nil || fanIn.branch("performance").status != journal.BranchFailed {
		t.Fatalf("pending fan-in = %+v, want failed performance outcome restored", fanIn)
	}
	completed := reconstructStageOutputs(events, machine)
	if got, ok, branchRef, absent := resolveBranchInput(
		"fan.security.review-security.summary", machine, completed, fanIn,
	); !branchRef || absent || !ok || got != "security" {
		t.Errorf("resumed security input = %#v (ok=%v branchRef=%v absent=%v), want security", got, ok, branchRef, absent)
	}
	if _, ok, branchRef, absent := resolveBranchInput(
		"fan.performance.summary", machine, completed, fanIn,
	); !branchRef || !absent || ok {
		t.Errorf("resumed failed branch = ok=%v branchRef=%v absent=%v, want absent", ok, branchRef, absent)
	}
}

func TestPendingFanInClearsAfterJoinGateEvaluation(t *testing.T) {
	machine := agenticGateFanInMachine(t)
	events := []journal.Event{
		{Type: journal.EventParallelFinished, Parallel: "fan", Target: "collate", Completeness: []journal.BranchOutcome{
			{Branch: 1, Name: "security", Status: journal.BranchSucceeded},
			{Branch: 2, Name: "performance", Status: journal.BranchSucceeded},
		}},
		{Type: journal.EventGateEvaluated, Gate: "collate", Verdict: gate.OutcomePass, Target: workflow.TerminalComplete},
	}
	if fanIn := pendingFanIn(events, machine); fanIn != nil {
		t.Fatalf("pending fan-in = %+v, want nil after join gate evaluation", fanIn)
	}
}
