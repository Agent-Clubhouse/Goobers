package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

func interventionTestMachine(t *testing.T, evaluator apiv1.EvaluatorKind) *workflow.Machine {
	t.Helper()
	return interventionTestMachineNamed(t, "intervention", evaluator, nil)
}

func interventionTestMachineNamed(t *testing.T, name string, evaluator apiv1.EvaluatorKind, approvers []string) *workflow.Machine {
	t.Helper()
	return interventionTestMachineWithPassTarget(t, name, evaluator, approvers, "finish")
}

func interventionTerminalTestMachine(t *testing.T, evaluator apiv1.EvaluatorKind) *workflow.Machine {
	t.Helper()
	return interventionTestMachineWithPassTarget(t, "terminal-intervention", evaluator, nil, workflow.TerminalComplete)
}

func interventionTestMachineWithPassTarget(
	t *testing.T,
	name string,
	evaluator apiv1.EvaluatorKind,
	approvers []string,
	passTarget string,
) *workflow.Machine {
	t.Helper()
	review := apiv1.Gate{
		Name: "review", Evaluator: evaluator,
		Branches: map[string]string{
			"pass":          passTarget,
			"fail":          workflow.TargetEscalate,
			"needs-changes": workflow.TargetEscalate,
		},
	}
	switch evaluator {
	case apiv1.EvaluatorAgentic:
		review.Agentic = &apiv1.AgenticGate{Goober: "reviewer"}
	case apiv1.EvaluatorHuman:
		review.Human = &apiv1.HumanGate{Approvers: approvers}
	default:
		t.Fatalf("unsupported test evaluator %q", evaluator)
	}
	tasks := []apiv1.Task{{
		Name: "implement", Type: apiv1.TaskDeterministic, Goal: "implement",
		Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: "review",
	}}
	if passTarget != workflow.TerminalComplete {
		tasks = append(tasks, apiv1.Task{
			Name: "finish", Type: apiv1.TaskDeterministic, Goal: "finish",
			Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: workflow.TerminalComplete,
		})
	}
	machine, err := workflow.Compile(workflow.Definition{
		Name: name, Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "example", Start: "implement",
			Tasks: tasks,
			Gates: []apiv1.Gate{review},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	return machine
}

func interventionTwoGateMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	machine, err := workflow.Compile(workflow.Definition{
		Name: "two-gate-intervention", Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "example", Start: "implement",
			Tasks: []apiv1.Task{
				{
					Name: "implement", Type: apiv1.TaskDeterministic, Goal: "implement",
					Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: "review",
				},
				{
					Name: "finish", Type: apiv1.TaskDeterministic, Goal: "finish",
					Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: workflow.TerminalComplete,
				},
			},
			Gates: []apiv1.Gate{
				{
					Name: "review", Evaluator: apiv1.EvaluatorAgentic,
					Agentic: &apiv1.AgenticGate{Goober: "reviewer"},
					Branches: map[string]string{
						"pass":          "approval",
						"fail":          workflow.TargetEscalate,
						"needs-changes": workflow.TargetEscalate,
					},
				},
				{
					Name: "approval", Evaluator: apiv1.EvaluatorHuman,
					Human: &apiv1.HumanGate{},
					Branches: map[string]string{
						"pass": "finish",
						"fail": workflow.TargetEscalate,
					},
				},
			},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}

	return machine
}

func interventionParallelGateMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	machine, err := workflow.Compile(workflow.Definition{
		Name: "parallel-intervention", Version: 1, DSLVersion: "2.0",
		Spec: apiv1.WorkflowSpec{
			Gaggle: "example", Start: "fan",
			Tasks: []apiv1.Task{
				{
					Name: "branch-work", Type: apiv1.TaskDeterministic, Goal: "branch work",
					Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: "review",
				},
				{
					Name: "other-work", Type: apiv1.TaskDeterministic, Goal: "other work",
					Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: workflow.TargetJoin,
				},
				{
					Name: "collate", Type: apiv1.TaskDeterministic, Goal: "collate",
					Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: workflow.TerminalComplete,
				},
			},
			Gates: []apiv1.Gate{{
				Name: "review", Evaluator: apiv1.EvaluatorAgentic,
				Agentic: &apiv1.AgenticGate{Goober: "reviewer", Workspace: apiv1.WorkspaceScratch},
				Branches: map[string]string{
					"pass":          workflow.TargetJoin,
					"fail":          workflow.TargetEscalate,
					"needs-changes": workflow.TargetEscalate,
				},
			}},
			Parallels: []apiv1.Parallel{{
				Name: "fan", Join: "collate",
				FailurePolicy: apiv1.BranchContinueOnError,
				Branches: []apiv1.Branch{
					{Name: "review", Start: "branch-work"},
					{Name: "other", Start: "other-work"},
				},
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	return machine
}

type interventionDeterministic struct{}

func (interventionDeterministic) Run(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func newInterventionServiceTestRun(
	t *testing.T,
	machine *workflow.Machine,
	runID string,
	events []journal.Event,
) (*runInterventionService, string) {
	t.Helper()
	return newInterventionServiceTestRunWithDeterministic(t, machine, runID, events, interventionDeterministic{})
}

func newInterventionServiceTestRunWithDeterministic(
	t *testing.T,
	machine *workflow.Machine,
	runID string,
	events []journal.Event,
	deterministic invoke.Deterministic,
) (*runInterventionService, string) {
	t.Helper()
	return newDriftedInterventionServiceTestRun(t, machine, machine, false, runID, events, deterministic)
}

// newDriftedInterventionServiceTestRun creates a run pinned to pinnedMachine
// while the definitions registry serves servedMachine — the #3376 shape: the
// workflow config was edited (or replaced wholesale) between the run's start
// and the operator's intervention. When snapshotDefinition is true the run
// carries the trusted pinned-definition input that Runner.Start journals;
// false reproduces a pre-snapshot run whose pin cannot be reconstructed.
func newDriftedInterventionServiceTestRun(
	t *testing.T,
	pinnedMachine, servedMachine *workflow.Machine,
	snapshotDefinition bool,
	runID string,
	events []journal.Event,
	deterministic invoke.Deterministic,
) (*runInterventionService, string) {
	t.Helper()
	layout := instance.NewLayout(t.TempDir())
	scoped := layout.ForGaggle("example")
	instanceLog, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instanceLog.Close() })
	manager, err := worktree.NewManager(scoped.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	runRunner, err := runner.New(runner.Config{
		NewDeterministic: func(runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Deterministic, error) {
			return deterministic, nil
		},
		Automated:  gate.NewAutomatedEvaluator(),
		Worktrees:  manager,
		ScratchDir: filepath.Join(scoped.WorkcopiesDir(), "scratch"),
		RunsDir:    scoped.RunsDir(),
		FinalizeTerminal: func(runID string, _ journal.RunPhase) error {
			return releaseClaimsForRun(layout, instanceLog, runID)
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var inputs map[string][]byte
	var createOpts []journal.Option
	if snapshotDefinition {
		definition, err := json.Marshal(pinnedMachine.Def)
		if err != nil {
			t.Fatal(err)
		}
		inputs = map[string][]byte{journal.PinnedWorkflowDefinitionInputName: definition}
		createOpts = append(createOpts, journal.WithInputIntegrity(map[string]apiv1.Integrity{
			journal.PinnedWorkflowDefinitionInputName: apiv1.IntegrityTrusted,
		}))
	}
	run, err := journal.Create(scoped.RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: pinnedMachine.Def.Name, WorkflowVersion: pinnedMachine.Def.Version,
		WorkflowDigest: pinnedMachine.Digest(), Gaggle: "example",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, inputs, createOpts...)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	key := localscheduler.WorkflowIdentity{Gaggle: "example", Workflow: servedMachine.Def.Name}
	runners := map[string]*runner.Runner{"example": runRunner}
	runnerRegistry := newDaemonRunnerRegistry()
	runnerRegistry.Replace(runners)
	service := &runInterventionService{
		layout: layout,
		definitions: newInterventionDefinitionRegistry(interventionDefinitionSet{
			runners:       runners,
			machines:      map[localscheduler.WorkflowIdentity]*workflow.Machine{key: servedMachine},
			gooberDigests: map[localscheduler.WorkflowIdentity]string{key: ""},
			repoRefs: map[localscheduler.WorkflowIdentity]apiv1.RepoRef{
				key: {Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "repo", Branch: "main"},
			},
		}),
		runnerRegistry: runnerRegistry,
		instanceLog:    instanceLog,
	}
	service.AttachScheduler(localscheduler.New([]localscheduler.WorkflowEntry{{
		Workflow: servedMachine.Def.Name,
		Gaggle:   "example",
		Readiness: apiv1.ReadinessConditions{
			MaxConcurrentRuns: 1,
		},
	}}, instanceLog))
	return service, filepath.Join(scoped.RunsDir(), runID)
}

func TestRunInterventionTerminalBranchesComplete(t *testing.T) {
	for _, action := range []string{"approve", "override"} {
		t.Run(action, func(t *testing.T) {
			machine := interventionTerminalTestMachine(t, apiv1.EvaluatorAgentic)
			runID := "run-terminal-" + action
			service, runDir := newInterventionServiceTestRun(t, machine, runID, []journal.Event{
				{Type: journal.EventGateStarted, Gate: "review"},
				{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
				{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
			})
			input := httpapi.InterventionRequest{
				RunID: runID, Stage: "review", Actor: "operator", Decision: "pass",
				Rationale: "accepted terminal outcome",
			}

			var (
				result httpapi.InterventionResult
				err    error
			)
			if action == "approve" {
				result, err = service.Approve(context.Background(), input)
			} else {
				result, err = service.Override(context.Background(), input)
			}
			if err != nil {
				t.Fatalf("%s: %v", action, err)
			}
			if result.Phase != string(journal.PhaseCompleted) {
				t.Fatalf("result = %+v, want completed", result)
			}

			reader, err := journal.OpenRead(runDir)
			if err != nil {
				t.Fatal(err)
			}
			events, err := reader.Events()
			if err != nil {
				t.Fatal(err)
			}
			var resumed journal.Event
			for _, event := range events {
				if event.Type == journal.EventRunResumed {
					resumed = event
				}
			}

			if resumed.Type != journal.EventRunResumed ||
				resumed.Action != action ||
				resumed.Target != "" ||
				!resumed.Complete {
				t.Fatalf("run.resumed = %+v", resumed)
			}
		})
	}
}

func TestRunInterventionOverrideReopensParallelBranchAtJoin(t *testing.T) {
	machine := interventionParallelGateMachine(t)
	service, runDir := newInterventionServiceTestRun(t, machine, "run-parallel-override", []journal.Event{
		{Type: journal.EventParallelStarted, Parallel: "fan"},
		{Type: journal.EventBranchStarted, Parallel: "fan", Branch: 1, BranchName: "review", Stage: "branch-work"},
		{Type: journal.EventStageStarted, Branch: 1, Stage: "branch-work", Attempt: 1},
		{Type: journal.EventStageFinished, Branch: 1, Stage: "branch-work", Attempt: 1, Status: string(apiv1.ResultSuccess)},
		{Type: journal.EventGateStarted, Branch: 1, Gate: "review"},
		{Type: journal.EventGateEvaluated, Branch: 1, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventBranchFinished, Parallel: "fan", Branch: 1, BranchName: "review", BranchStatus: journal.BranchFailed},
		{Type: journal.EventBranchFinished, Parallel: "fan", Branch: 2, BranchName: "other", BranchStatus: journal.BranchCancelled},
		{Type: journal.EventParallelFinished, Parallel: "fan", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	})

	result, err := service.Override(context.Background(), httpapi.InterventionRequest{
		RunID: "run-parallel-override", Stage: "review", Actor: "operator",
		Decision: "pass", Rationale: "accepted branch result",
	})
	if err != nil {
		t.Fatalf("Override: %v", err)
	}
	if result.Phase != string(journal.PhaseCompleted) {
		t.Fatalf("result = %+v, want completed", result)
	}

	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var resumed journal.Event
	otherStarted := false
	for _, event := range events {
		if event.Type == journal.EventRunResumed {
			resumed = event
		}
		if event.Type == journal.EventBranchStarted && event.Branch == 2 {
			otherStarted = true
		}
	}
	if resumed.Target != workflow.TargetJoin || resumed.Parallel != "fan" || resumed.Branch != 1 {
		t.Fatalf("run.resumed = %+v", resumed)
	}
	if !otherStarted {
		t.Fatal("parallel sibling was not resumed after the approved branch joined")
	}
}

func TestRunInterventionOverrideResumesAndJournalsAction(t *testing.T) {
	machine := interventionTestMachine(t, apiv1.EvaluatorAgentic)
	service, runDir := newInterventionServiceTestRun(t, machine, "run-override", []journal.Event{
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)},
		{Type: journal.EventGateStarted, Gate: "review"},
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	})
	result, err := service.Override(context.Background(), httpapi.InterventionRequest{
		RunID: "run-override", Stage: "review", Actor: "operator",
		Decision: "pass", Rationale: "accepted after manual review",
	})
	if err != nil {
		t.Fatalf("Override: %v", err)
	}
	if result.Phase != string(journal.PhaseCompleted) {
		t.Fatalf("result = %+v, want completed", result)
	}
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var resumed journal.Event
	for _, event := range events {
		if event.Type == journal.EventRunResumed {
			resumed = event
		}
	}
	if resumed.Type != journal.EventRunResumed ||
		resumed.Actor != "operator" ||
		resumed.Target != "finish" ||
		resumed.Action != "override" ||
		resumed.Gate != "review" ||
		resumed.Decision != "pass" ||
		resumed.Rationale != "accepted after manual review" {
		t.Fatalf("run.resumed = %+v", resumed)
	}
}

func TestRunInterventionIdempotencyReplaysCompletedAction(t *testing.T) {
	machine := interventionTestMachine(t, apiv1.EvaluatorAgentic)
	service, runDir := newInterventionServiceTestRun(t, machine, "run-idempotent", []journal.Event{
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)},
		{Type: journal.EventGateStarted, Gate: "review"},
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	})
	input := httpapi.InterventionRequest{
		RunID: "run-idempotent", Stage: "review", Actor: "operator", Decision: "pass",
		Rationale: "accepted after manual review", IdempotencyKey: "same-request",
	}
	first, err := service.Override(context.Background(), input)
	if err != nil {
		t.Fatalf("first Override: %v", err)
	}
	second, err := service.Override(context.Background(), input)
	if err != nil {
		t.Fatalf("replayed Override: %v", err)
	}
	if second != first || first.JournalSeq == 0 {
		t.Fatalf("results = first %+v, second %+v", first, second)
	}

	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	markers, resumed := 0, 0
	for _, event := range events {
		if interventionMarkerKey(event) == input.IdempotencyKey {
			markers++
		}
		if event.Type == journal.EventRunResumed && event.Action == "override" {
			resumed++
		}
	}
	if markers != 1 || resumed != 1 {
		t.Fatalf("markers = %d, resumed = %d; want one each", markers, resumed)
	}

	input.Rationale = "different action"
	_, err = service.Override(context.Background(), input)
	var interventionErr *httpapi.InterventionError
	if !errors.As(err, &interventionErr) || interventionErr.Code != "idempotency_key_reused" {
		t.Fatalf("key reuse error = %#v, want idempotency_key_reused", err)
	}
}

func TestRunInterventionApproveResolvesPausedHumanGate(t *testing.T) {
	machine := interventionTestMachine(t, apiv1.EvaluatorHuman)
	service, runDir := newInterventionServiceTestRun(t, machine, "run-approve", []journal.Event{
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)},
		{Type: journal.EventGateStarted, Gate: "review"},
		{Type: journal.EventGatePaused, Gate: "review"},
	})
	result, err := service.Approve(context.Background(), httpapi.InterventionRequest{
		RunID: "run-approve", Stage: "review", Actor: "approver", Decision: "pass",
	})
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if result.Phase != string(journal.PhaseCompleted) {
		t.Fatalf("result = %+v, want completed", result)
	}
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var evaluated journal.Event
	for _, event := range events {
		if event.Type == journal.EventGateEvaluated {
			evaluated = event
		}
	}
	if evaluated.Gate != "review" || evaluated.Verdict != "pass" || evaluated.Target != "finish" || evaluated.Actor != "approver" {
		t.Fatalf("gate.evaluated = %+v", evaluated)
	}
}

// interventionGoalDriftMachine compiles the same human-gated workflow shape
// with a caller-chosen goal for the implement task — two goals, two digests,
// one workflow name: the #3376 residual case, a legitimate semantic edit
// landing between a run's start and an operator's intervention.
func interventionGoalDriftMachine(t *testing.T, goal string) *workflow.Machine {
	t.Helper()
	machine, err := workflow.Compile(workflow.Definition{
		Name: "intervention", Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "example", Start: "implement",
			Tasks: []apiv1.Task{
				{
					Name: "implement", Type: apiv1.TaskDeterministic, Goal: goal,
					Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: "review",
				},
				{
					Name: "finish", Type: apiv1.TaskDeterministic, Goal: "finish",
					Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: workflow.TerminalComplete,
				},
			},
			Gates: []apiv1.Gate{{
				Name: "review", Evaluator: apiv1.EvaluatorHuman,
				Human: &apiv1.HumanGate{},
				Branches: map[string]string{
					"pass": "finish",
					"fail": workflow.TargetEscalate,
				},
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	return machine
}

// TestRunInterventionApproveResumesDriftedRunFromPinnedDefinition is the
// #3376 residual-case regression: the workflow was legitimately edited after
// this run paused at its human gate, so the served machine's digest no longer
// matches the run's WF-016 pin. Before the fix the operator's approve was
// executed against the current machine and refuseResume destroyed the paused
// run (terminal failed, resume_refused_digest_mismatch); with the pinned
// definition snapshot journaled at Start, resolve() must reconstruct the
// historical machine and the approval must walk the run to completion.
func TestRunInterventionApproveResumesDriftedRunFromPinnedDefinition(t *testing.T) {
	pinned := interventionGoalDriftMachine(t, "implement")
	edited := interventionGoalDriftMachine(t, "implement, but the goal was edited mid-run")
	if pinned.Digest() == edited.Digest() {
		t.Fatal("fixture machines must have drifted digests")
	}
	service, runDir := newDriftedInterventionServiceTestRun(t, pinned, edited, true, "run-drifted-approve", []journal.Event{
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)},
		{Type: journal.EventGateStarted, Gate: "review"},
		{Type: journal.EventGatePaused, Gate: "review"},
	}, interventionDeterministic{})

	result, err := service.Approve(context.Background(), httpapi.InterventionRequest{
		RunID: "run-drifted-approve", Stage: "review", Actor: "approver", Decision: "pass",
	})
	if err != nil {
		t.Fatalf("Approve: %v — a drifted-but-snapshotted run must resume against its pinned definition, not refuse", err)
	}
	if result.Phase != string(journal.PhaseCompleted) {
		t.Fatalf("result = %+v, want completed", result)
	}
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var finished journal.Event
	for _, event := range events {
		if event.Type == journal.EventRunFinished {
			finished = event
		}
	}
	if finished.Status != string(journal.PhaseCompleted) || finished.Error != nil {
		t.Fatalf("run.finished = %+v, want clean completed terminal", finished)
	}
}

// TestRunInterventionTerminalApproveResumesDriftedRunFromPinnedDefinition
// covers the second intervention entrypoint, ResumeFromTerminal: an escalated
// run whose workflow was edited afterwards must still be reopenable by a
// human against its pinned definition (WF-016 pin verified against the
// reconstructed machine, not the drifted current one).
func TestRunInterventionTerminalApproveResumesDriftedRunFromPinnedDefinition(t *testing.T) {
	pinned := interventionGoalDriftMachine(t, "implement")
	edited := interventionGoalDriftMachine(t, "implement, but the goal was edited mid-run")
	service, runDir := newDriftedInterventionServiceTestRun(t, pinned, edited, true, "run-drifted-terminal", []journal.Event{
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)},
		{Type: journal.EventGateStarted, Gate: "review"},
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	}, interventionDeterministic{})

	result, err := service.Approve(context.Background(), httpapi.InterventionRequest{
		RunID: "run-drifted-terminal", Stage: "review", Actor: "approver", Decision: "pass",
	})
	if err != nil {
		t.Fatalf("Approve: %v — terminal resume must verify the pin against the reconstructed machine", err)
	}
	if result.Phase != string(journal.PhaseCompleted) {
		t.Fatalf("result = %+v, want completed", result)
	}
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var finished journal.Event
	for _, event := range events {
		if event.Type == journal.EventRunFinished {
			finished = event
		}
	}
	if finished.Status != string(journal.PhaseCompleted) {
		t.Fatalf("final run.finished = %+v, want completed", finished)
	}
}

// TestRunInterventionApproveStillRefusesDriftedRunWithoutSnapshot pins the
// tamper-evidence property (#3376 scope: refusal MUST remain when the old
// definition is unavailable): when the run's pin cannot be reconstructed from
// a trusted snapshot, the intervention proceeds against the current machine
// and the runner's WF-016 verification refuses exactly as before.
func TestRunInterventionApproveStillRefusesDriftedRunWithoutSnapshot(t *testing.T) {
	pinned := interventionGoalDriftMachine(t, "implement")
	edited := interventionGoalDriftMachine(t, "implement, but the goal was edited mid-run")
	service, runDir := newDriftedInterventionServiceTestRun(t, pinned, edited, false, "run-drifted-unpinned", []journal.Event{
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)},
		{Type: journal.EventGateStarted, Gate: "review"},
		{Type: journal.EventGatePaused, Gate: "review"},
	}, interventionDeterministic{})

	result, err := service.Approve(context.Background(), httpapi.InterventionRequest{
		RunID: "run-drifted-unpinned", Stage: "review", Actor: "approver", Decision: "pass",
	})
	if err != nil {
		t.Fatalf("Approve: %v — a handled WF-016 refusal is a result, not an error", err)
	}
	if result.Phase != string(journal.PhaseFailed) {
		t.Fatalf("result = %+v, want the WF-016 refusal's canonical failed terminal", result)
	}
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var finished journal.Event
	for _, event := range events {
		if event.Type == journal.EventRunFinished {
			finished = event
		}
	}
	if finished.Error == nil || finished.Error.Code != "resume_refused_digest_mismatch" {
		t.Fatalf("run.finished error = %+v, want resume_refused_digest_mismatch — tamper evidence must survive the pinned-definition fallback", finished.Error)
	}
}

func TestRunInterventionRejectsInvalidStateAndInput(t *testing.T) {
	machine := interventionTestMachine(t, apiv1.EvaluatorAgentic)
	service, _ := newInterventionServiceTestRun(t, machine, "run-complete", []journal.Event{
		{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)},
	})

	_, err := service.Override(context.Background(), httpapi.InterventionRequest{
		RunID: "run-complete", Stage: "review", Actor: "operator",
	})
	var interventionErr *httpapi.InterventionError
	if !errors.As(err, &interventionErr) || interventionErr.Status != http.StatusBadRequest || interventionErr.Code != "rationale_required" {
		t.Fatalf("missing-rationale error = %#v", err)
	}

	_, err = service.RerunStage(context.Background(), httpapi.InterventionRequest{
		RunID: "run-complete", Stage: "implement", Actor: "operator", InstructionAddendum: "try again",
	})
	if !errors.As(err, &interventionErr) || interventionErr.Status != http.StatusConflict || interventionErr.Code != "run_not_escalated" {
		t.Fatalf("terminal rerun error = %#v", err)
	}

	_, err = service.Approve(context.Background(), httpapi.InterventionRequest{
		RunID: "../escape", Stage: "review", Actor: "operator",
	})
	if !errors.As(err, &interventionErr) || interventionErr.Status != http.StatusBadRequest || interventionErr.Code != "invalid_run_id" {
		t.Fatalf("invalid-run error = %#v", err)
	}
}

func TestRunInterventionApproveRejectsUnauthorizedTerminalHumanActor(t *testing.T) {
	machine := interventionTestMachineNamed(t, "restricted-intervention", apiv1.EvaluatorHuman, []string{"allowed"})
	service, runDir := newInterventionServiceTestRun(t, machine, "run-restricted", []journal.Event{
		{Type: journal.EventGateStarted, Gate: "review"},
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	})

	_, err := service.Approve(context.Background(), httpapi.InterventionRequest{
		RunID: "run-restricted", Stage: "review", Actor: "intruder", Decision: "pass",
	})
	var interventionErr *httpapi.InterventionError
	if !errors.As(err, &interventionErr) ||
		interventionErr.Status != http.StatusForbidden ||
		interventionErr.Code != "approval_forbidden" {
		t.Fatalf("Approve error = %#v, want approval_forbidden", err)
	}

	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	if got := events[len(events)-1]; got.Type != journal.EventRunFinished || got.Status != string(journal.PhaseEscalated) {
		t.Fatalf("last event = %+v, want original escalated terminal", got)
	}
}

func TestRunInterventionUsesDefinitionsReplacedAfterReload(t *testing.T) {
	initial := interventionTestMachineNamed(t, "initial-intervention", apiv1.EvaluatorAgentic, nil)
	service, _ := newInterventionServiceTestRun(t, initial, "run-initial", []journal.Event{
		{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)},
	})
	reloaded := interventionTestMachineNamed(t, "reloaded-intervention", apiv1.EvaluatorAgentic, nil)
	snapshot := service.definitions.Snapshot()
	key := localscheduler.WorkflowIdentity{Gaggle: "example", Workflow: reloaded.Def.Name}
	service.definitions.Replace(interventionDefinitionSet{
		runners:       snapshot.runners,
		machines:      map[localscheduler.WorkflowIdentity]*workflow.Machine{key: reloaded},
		gooberDigests: map[localscheduler.WorkflowIdentity]string{key: ""},
		repoRefs: map[localscheduler.WorkflowIdentity]apiv1.RepoRef{
			key: {Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "repo", Branch: "main"},
		},
	})
	if err := service.scheduler.Load().Reload([]localscheduler.WorkflowEntry{{
		Workflow: reloaded.Def.Name,
		Gaggle:   "example",
		Readiness: apiv1.ReadinessConditions{
			MaxConcurrentRuns: 1,
		},
	}}, nil, time.Now(), "old", "new"); err != nil {
		t.Fatal(err)
	}

	const runID = "run-after-reload"
	run, err := journal.Create(service.layout.ForGaggle("example").RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: reloaded.Def.Name, WorkflowVersion: reloaded.Def.Version,
		WorkflowDigest: reloaded.Digest(), Gaggle: "example",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []journal.Event{
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	} {
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := service.Override(context.Background(), httpapi.InterventionRequest{
		RunID: runID, Stage: "review", Actor: "operator",
		Decision: "pass", Rationale: "reviewed after reload",
	})
	if err != nil {
		t.Fatalf("Override: %v", err)
	}
	if result.Phase != string(journal.PhaseCompleted) {
		t.Fatalf("result = %+v, want post-reload run completed", result)
	}
}

type blockingInterventionDeterministic struct {
	started chan struct{}
}

func (d *blockingInterventionDeterministic) Run(ctx context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	close(d.started)
	<-ctx.Done()
	return apiv1.ResultEnvelope{}, ctx.Err()
}

type releasableInterventionDeterministic struct {
	started chan struct{}
	release chan struct{}
}

func (d *releasableInterventionDeterministic) Run(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	close(d.started)
	<-d.release
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func TestRunInterventionRegistersLiveOwnerForCancellation(t *testing.T) {
	machine := interventionTestMachine(t, apiv1.EvaluatorAgentic)
	deterministic := &blockingInterventionDeterministic{started: make(chan struct{})}
	service, _ := newInterventionServiceTestRunWithDeterministic(t, machine, "run-cancellable", []journal.Event{
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	}, deterministic)

	result, err := service.AcceptOverride(context.Background(), context.Background(), httpapi.InterventionRequest{
		RunID: "run-cancellable", Stage: "review", Actor: "operator",
		Decision: "pass", Rationale: "continue under observation", IdempotencyKey: "cancellable-override",
	})
	if err != nil {
		t.Fatalf("AcceptOverride: %v", err)
	}
	if result.JournalSeq == 0 {
		t.Fatalf("accepted result = %+v, want durable journal position", result)
	}
	select {
	case <-deterministic.started:
	case <-time.After(5 * time.Second):
		t.Fatal("intervention did not start resumed stage")
	}

	response := executeCancelRequest(service.runnerRegistry, nil, cancelRequest{
		RunID: "run-cancellable", Gaggle: "example", Actor: "operator",
	}, time.Now())
	if response.Code != cancelCodeAborted || response.Phase != string(journal.PhaseAborted) {
		t.Fatalf("cancel response = %+v, want aborted", response)
	}
	deadline := time.Now().Add(5 * time.Second)
	for service.interventionActive("run-cancellable") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if service.interventionActive("run-cancellable") {
		t.Fatal("accepted intervention did not stop after cancellation")
	}
}

func TestRunInterventionReacquiresClaimsAndAdmissionUntilTerminal(t *testing.T) {
	machine := interventionTestMachine(t, apiv1.EvaluatorAgentic)
	deterministic := &releasableInterventionDeterministic{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	service, _ := newInterventionServiceTestRunWithDeterministic(t, machine, "run-reserved", []journal.Event{
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	}, deterministic)
	if err := service.instanceLog.Append(journal.Event{
		Type: journal.EventClaimAcquired, Name: "466", Gaggle: "example",
		RunID: "run-reserved", Workflow: machine.Def.Name,
		Runner: map[string]any{"claimProvider": "github", "claimExternalId": "466"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.instanceLog.Append(journal.Event{
		Type: journal.EventClaimReleased, Name: "466", Gaggle: "example",
		RunID: "run-reserved", Workflow: machine.Def.Name,
	}); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := service.Override(context.Background(), httpapi.InterventionRequest{
			RunID: "run-reserved", Stage: "review", Actor: "operator",
			Decision: "pass", Rationale: "resume safely",
		})
		done <- err
	}()
	select {
	case <-deterministic.started:
	case <-time.After(5 * time.Second):
		t.Fatal("resumed stage did not start")
	}

	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(service.layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	key := localscheduler.ClaimKey{Gaggle: "example", Provider: "github", ExternalID: "466"}
	if entry, held := ledger.LookupScoped(key); !held || entry.RunID != "run-reserved" {
		t.Fatalf("reacquired claim = (%+v, %v)", entry, held)
	}
	if release, ok, reason := service.scheduler.Load().ReserveContinuation("competing-run", "example", machine.Def.Name); ok {
		release()
		t.Fatal("competing run acquired admission while intervention was active")
	} else if reason != localscheduler.ReasonMaxParallel {
		t.Fatalf("competing admission refusal = %q", reason)
	}

	close(deterministic.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Override: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("intervention did not finish")
	}
	reopened, err := localscheduler.OpenClaimLedger(filepath.Join(service.layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if entry, held := reopened.LookupScoped(key); held {
		t.Fatalf("terminal run retained claim: %+v", entry)
	}
	if release, ok, reason := service.scheduler.Load().ReserveContinuation("next-run", "example", machine.Def.Name); !ok {
		t.Fatalf("terminal run retained admission: %s", reason)
	} else {
		release()
	}
}

func TestRunInterventionProtectsReacquiredClaimsBeforeJournalResume(t *testing.T) {
	machine := interventionTestMachine(t, apiv1.EvaluatorAgentic)
	service, _ := newInterventionServiceTestRun(t, machine, "run-recovery-window", []journal.Event{
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	})
	if err := service.instanceLog.Append(journal.Event{
		Type: journal.EventClaimAcquired, Name: "466", Gaggle: "example",
		RunID: "run-recovery-window", Workflow: machine.Def.Name,
		Runner: map[string]any{"claimProvider": "github", "claimExternalId": "466"},
	}); err != nil {
		t.Fatal(err)
	}
	resolved, err := service.resolve("run-recovery-window")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := service.beginExecution(resolved, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = lease.releaseReacquiredClaims()
		lease.Close()
	}()

	type recoveryResult struct {
		released []localscheduler.ClaimEntry
		err      error
	}
	done := make(chan recoveryResult, 1)
	go func() {
		released, err := recoverClaims(service.layout, service.instanceLog, time.Now(), service.interventionActive)
		done <- recoveryResult{released: released, err: err}
	}()

	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if len(result.released) != 0 {
			t.Fatalf("recovery released active intervention claims: %+v", result.released)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("claim recovery did not finish")
	}

	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(service.layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	key := localscheduler.ClaimKey{Gaggle: "example", Provider: "github", ExternalID: "466"}
	if entry, held := ledger.LookupScoped(key); !held || entry.RunID != "run-recovery-window" {
		t.Fatalf("claim in pre-resume window = (%+v, %v)", entry, held)
	}
}

func TestRunInterventionRetainsResourcesAcrossAnotherHumanPause(t *testing.T) {
	machine := interventionTwoGateMachine(t)
	service, _ := newInterventionServiceTestRun(t, machine, "run-repaused", []journal.Event{
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	})
	if err := service.instanceLog.Append(journal.Event{
		Type: journal.EventClaimAcquired, Name: "466", Gaggle: "example",
		RunID: "run-repaused", Workflow: machine.Def.Name,
		Runner: map[string]any{"claimProvider": "github", "claimExternalId": "466"},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := service.Override(context.Background(), httpapi.InterventionRequest{
		RunID: "run-repaused", Stage: "review", Actor: "operator",
		Decision: "pass", Rationale: "continue to approval",
	})
	if err != nil {
		t.Fatalf("Override: %v", err)
	}
	if result.Phase != string(journal.PhaseRunning) || result.State != "approval" {
		t.Fatalf("override result = %+v, want paused at approval", result)
	}

	key := localscheduler.ClaimKey{Gaggle: "example", Provider: "github", ExternalID: "466"}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(service.layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if entry, held := ledger.LookupScoped(key); !held || entry.RunID != "run-repaused" {
		t.Fatalf("claim while re-paused = (%+v, %v)", entry, held)
	}
	if release, ok, _ := service.scheduler.Load().ReserveContinuation("competing-run", "example", machine.Def.Name); ok {
		release()
		t.Fatal("re-paused intervention released workflow admission")
	}

	result, err = service.Approve(context.Background(), httpapi.InterventionRequest{
		RunID: "run-repaused", Stage: "approval", Actor: "approver", Decision: "pass",
	})
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if result.Phase != string(journal.PhaseCompleted) {
		t.Fatalf("approval result = %+v, want completed", result)
	}
	reopened, err := localscheduler.OpenClaimLedger(filepath.Join(service.layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if entry, held := reopened.LookupScoped(key); held {
		t.Fatalf("completed re-paused run retained claim: %+v", entry)
	}
	if release, ok, reason := service.scheduler.Load().ReserveContinuation("next-run", "example", machine.Def.Name); !ok {
		t.Fatalf("completed re-paused run retained admission: %s", reason)
	} else {
		release()
	}
}

func TestRunInterventionRejectsDelayedDuplicateFromPriorTerminalSegment(t *testing.T) {
	machine := interventionTestMachine(t, apiv1.EvaluatorAgentic)
	service, runDir := newInterventionServiceTestRun(t, machine, "run-delayed-duplicate", []journal.Event{
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	})
	stale, err := service.resolve("run-delayed-duplicate")
	if err != nil {
		t.Fatal(err)
	}

	recovered, _, err := journal.Recover(runDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []journal.Event{
		{
			Type: journal.EventRunResumed, Status: string(journal.PhaseEscalated),
			Actor: "first-operator", Action: "override", Gate: "review", Decision: "pass", Target: "implement",
			WorkflowVersion: machine.Def.Version, WorkflowDigest: machine.Digest(),
		},
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	} {
		if err := recovered.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = service.execute(context.Background(), context.Background(), false, stale, true, "override", httpapi.InterventionRequest{}, func(ctx context.Context) (runner.Result, error) {
		return stale.runner.ResumeFromTerminal(ctx, runner.ResumeFromTerminalInput{
			RunID: stale.runID, Machine: stale.machine, GooberDigest: stale.gooberDigest, RepoRef: stale.repoRef,
			Target: "finish", Actor: "delayed-operator", Action: "override", Gate: "review", Decision: "pass",
			Rationale: "stale approval", ExpectedTerminalSeq: stale.terminalSeq,
		})
	})
	err = interventionExecutionError("override", err)
	var interventionErr *httpapi.InterventionError
	if !errors.As(err, &interventionErr) ||
		interventionErr.Status != http.StatusConflict ||
		interventionErr.Code != "terminal_generation_changed" {
		t.Fatalf("delayed duplicate error = %#v, want terminal_generation_changed", err)
	}

	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	resumes := 0
	for _, event := range events {
		if event.Type == journal.EventRunResumed {
			resumes++
		}
	}
	if resumes != 1 {
		t.Fatalf("run.resumed events = %d, want no stale duplicate", resumes)
	}
}

func TestRunInterventionRejectsGateEvidenceFromBeforeRerunSegment(t *testing.T) {
	machine := interventionTestMachine(t, apiv1.EvaluatorAgentic)
	service, _ := newInterventionServiceTestRun(t, machine, "run-rerun-failed", []journal.Event{
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
		{
			Type: journal.EventStageRerunRequested, Stage: "implement", Actor: "first-operator",
			InstructionAddendum: "try a different implementation",
		},
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 2},
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 2, Status: string(apiv1.ResultFailure)},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)},
	})

	_, err := service.Override(context.Background(), httpapi.InterventionRequest{
		RunID: "run-rerun-failed", Stage: "review", Actor: "second-operator",
		Decision: "pass", Rationale: "reuse the earlier review",
	})
	var interventionErr *httpapi.InterventionError
	if !errors.As(err, &interventionErr) ||
		interventionErr.Status != http.StatusConflict ||
		interventionErr.Code != "gate_not_evaluated" {
		t.Fatalf("Override error = %#v, want gate_not_evaluated", err)
	}
}

func TestRunInterventionRejectsClaimOwnedByAnotherRun(t *testing.T) {
	machine := interventionTestMachine(t, apiv1.EvaluatorAgentic)
	service, runDir := newInterventionServiceTestRun(t, machine, "run-conflicted", []journal.Event{
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	})
	if err := service.instanceLog.Append(journal.Event{
		Type: journal.EventClaimAcquired, Name: "466", Gaggle: "example",
		RunID: "run-conflicted", Workflow: machine.Def.Name,
		Runner: map[string]any{"claimProvider": "github", "claimExternalId": "466"},
	}); err != nil {
		t.Fatal(err)
	}

	key := localscheduler.ClaimKey{Gaggle: "example", Provider: "github", ExternalID: "466"}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(service.layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.ClaimScoped(key, "other-run", machine.Def.Name, time.Hour); err != nil || !ok {
		t.Fatalf("seed competing claim: ok=%v err=%v", ok, err)
	}

	_, err = service.Override(context.Background(), httpapi.InterventionRequest{
		RunID: "run-conflicted", Stage: "review", Actor: "operator",
		Decision: "pass", Rationale: "resume safely",
	})
	var interventionErr *httpapi.InterventionError
	if !errors.As(err, &interventionErr) || interventionErr.Status != http.StatusConflict || interventionErr.Code != "claim_unavailable" {
		t.Fatalf("Override error = %#v, want claim_unavailable", err)
	}
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == journal.EventRunResumed {
			t.Fatal("claim-conflicted run was resumed")
		}
	}

	if release, ok, reason := service.scheduler.Load().ReserveContinuation("probe-run", "example", machine.Def.Name); !ok {
		t.Fatalf("failed intervention leaked admission: %s", reason)
	} else {
		release()
	}
}

func TestRunInterventionUsesDurableClaimHistoryWhenInstanceJournalMissesAcquisition(t *testing.T) {
	machine := interventionTestMachine(t, apiv1.EvaluatorAgentic)
	service, _ := newInterventionServiceTestRun(t, machine, "run-durable-history", []journal.Event{
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	})
	key := localscheduler.ClaimKey{Gaggle: "example", Provider: "github", ExternalID: "466"}
	ledgerPath := filepath.Join(service.layout.SchedulerDir(), claimLedgerFileName)
	ledger, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.ClaimScoped(key, "run-durable-history", machine.Def.Name, time.Hour); err != nil || !ok {
		t.Fatalf("seed original claim: ok=%v err=%v", ok, err)
	}
	if err := ledger.ReleaseScoped(key, "run-durable-history"); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.ClaimScoped(key, "other-run", machine.Def.Name, time.Hour); err != nil || !ok {
		t.Fatalf("seed competing claim: ok=%v err=%v", ok, err)
	}

	_, err = service.Override(context.Background(), httpapi.InterventionRequest{
		RunID: "run-durable-history", Stage: "review", Actor: "operator",
		Decision: "pass", Rationale: "resume safely",
	})
	var interventionErr *httpapi.InterventionError
	if !errors.As(err, &interventionErr) || interventionErr.Code != "claim_unavailable" {
		t.Fatalf("Override error = %#v, want claim_unavailable", err)
	}
}

func TestRunInterventionResolvePrefersLiveOwnerAcrossReload(t *testing.T) {
	machine := interventionTestMachine(t, apiv1.EvaluatorAgentic)
	service, _ := newInterventionServiceTestRun(t, machine, "run-live-owner", []journal.Event{
		{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)},
	})
	snapshot := service.definitions.Snapshot()
	original := snapshot.runners["example"]
	reloaded := &runner.Runner{}
	snapshot.runners = map[string]*runner.Runner{"example": reloaded}
	service.definitions.Replace(snapshot)
	service.runnerRegistry.Replace(snapshot.runners)
	untrack := service.runnerRegistry.Track("run-live-owner", "", original)
	defer untrack()

	resolved, err := service.resolve("run-live-owner")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.runner != original {
		t.Fatalf("resolved runner = %p, want live owner %p", resolved.runner, original)
	}
}
