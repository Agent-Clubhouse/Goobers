package main

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

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
		review.Human = &apiv1.HumanGate{}
	default:
		t.Fatalf("unsupported test evaluator %q", evaluator)
	}
	machine, err := workflow.Compile(workflow.Definition{
		Name: "intervention", Version: 1,
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
	layout := instance.NewLayout(t.TempDir())
	scoped := layout.ForGaggle("example")
	manager, err := worktree.NewManager(scoped.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	runRunner, err := runner.New(runner.Config{
		NewDeterministic: func(runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Deterministic, error) {
			return interventionDeterministic{}, nil
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
	return &runInterventionService{
		layout:        layout,
		runners:       map[string]*runner.Runner{"example": runRunner},
		machines:      map[localscheduler.WorkflowIdentity]*workflow.Machine{key: machine},
		gooberDigests: map[localscheduler.WorkflowIdentity]string{key: ""},
		repoRefs: map[localscheduler.WorkflowIdentity]apiv1.RepoRef{
			key: {Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "repo", Branch: "main"},
		},
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
		resumed.Runner["interventionAction"] != "override" ||
		resumed.Runner["interventionGate"] != "review" ||
		resumed.Runner["interventionDecision"] != "pass" ||
		resumed.Runner["interventionRationale"] != "accepted after manual review" {
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
