package main

import (
	"context"
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
	review := apiv1.Gate{
		Name: "review", Evaluator: evaluator,
		Branches: map[string]string{
			"pass":          "finish",
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
	machine, err := workflow.Compile(workflow.Definition{
		Name: name, Version: 1,
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
			Gates: []apiv1.Gate{review},
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
	layout := instance.NewLayout(t.TempDir())
	scoped := layout.ForGaggle("example")
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
	})
	if err != nil {
		t.Fatal(err)
	}

	run, err := journal.Create(scoped.RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "example",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
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
	key := localscheduler.WorkflowIdentity{Gaggle: "example", Workflow: machine.Def.Name}
	runners := map[string]*runner.Runner{"example": runRunner}
	runnerRegistry := newDaemonRunnerRegistry()
	runnerRegistry.Replace(runners)
	return &runInterventionService{
		layout: layout,
		definitions: newInterventionDefinitionRegistry(interventionDefinitionSet{
			runners:       runners,
			machines:      map[localscheduler.WorkflowIdentity]*workflow.Machine{key: machine},
			gooberDigests: map[localscheduler.WorkflowIdentity]string{key: ""},
			repoRefs: map[localscheduler.WorkflowIdentity]apiv1.RepoRef{
				key: {Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "repo", Branch: "main"},
			},
		}),
		runnerRegistry: runnerRegistry,
	}, filepath.Join(scoped.RunsDir(), runID)
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
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := service.Override(ctx, httpapi.InterventionRequest{
		RunID: "run-override", Stage: "review", Actor: "operator",
		Decision: "pass", Rationale: "accepted after manual review",
	})
	if err != nil {
		t.Fatalf("Override: %v", err)
	}
	if result.Phase != string(journal.PhaseRunning) || result.State != "finish" {
		t.Fatalf("result = %+v, want running at finish", result)
	}
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	resumed := events[len(events)-1]
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
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := service.Override(cancelled, httpapi.InterventionRequest{
		RunID: runID, Stage: "review", Actor: "operator",
		Decision: "pass", Rationale: "reviewed after reload",
	})
	if err != nil {
		t.Fatalf("Override: %v", err)
	}
	if result.Phase != string(journal.PhaseRunning) || result.State != "finish" {
		t.Fatalf("result = %+v, want post-reload run reopened at finish", result)
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

func TestRunInterventionRegistersLiveOwnerForCancellation(t *testing.T) {
	machine := interventionTestMachine(t, apiv1.EvaluatorAgentic)
	deterministic := &blockingInterventionDeterministic{started: make(chan struct{})}
	service, _ := newInterventionServiceTestRunWithDeterministic(t, machine, "run-cancellable", []journal.Event{
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: workflow.TargetEscalate},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	}, deterministic)

	done := make(chan error, 1)
	go func() {
		_, err := service.Override(context.Background(), httpapi.InterventionRequest{
			RunID: "run-cancellable", Stage: "review", Actor: "operator",
			Decision: "pass", Rationale: "continue under observation",
		})
		done <- err
	}()
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
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Override after cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("intervention did not return after cancellation")
	}
}
