package runner

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/bandit"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

type banditCapturingDeterministic struct {
	env apiv1.InvocationEnvelope
}

func (d *banditCapturingDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	d.env = env
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

type banditCapturingGoober struct {
	env apiv1.InvocationEnvelope
}

func (g *banditCapturingGoober) Invoke(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	g.env = env
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (*banditCapturingGoober) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

func TestBanditConfigRejectsMismatchedDefaultGateLevel(t *testing.T) {
	m := experimentFixtureMachine(t, apiv1.BanditExperiment{
		Arms: []apiv1.BanditArm{
			{Name: "control", GateLevel: 2},
			{Name: "treatment", GateLevel: 2},
		},
		DefaultGateLevel: 1,
	})
	task, _ := m.Task("implement")
	_, _, err := banditConfig(m, task)
	if err == nil {
		t.Fatal("expected default gate mismatch to fail closed")
	}
}

func TestBanditConfigRequiresTaskToTransitionToGate(t *testing.T) {
	m := experimentFixtureMachine(t, apiv1.BanditExperiment{
		Arms: []apiv1.BanditArm{
			{Name: "control", GateLevel: 1},
			{Name: "treatment", GateLevel: 1},
		},
		DefaultGateLevel: 1,
	})
	// Get task and remove its Next field to simulate a task that doesn't transition to a gate
	task, _ := m.Task("implement")
	task.Next = ""
	_, _, err := banditConfig(m, task)
	if err == nil {
		t.Fatal("expected task without Next field to fail closed")
	}
	if !strings.Contains(err.Error(), "must transition to a gate") {
		t.Fatalf("expected error about gate transition, got: %v", err)
	}
}

func TestBanditConfigRequiresGateToExist(t *testing.T) {
	m := experimentFixtureMachine(t, apiv1.BanditExperiment{
		Arms: []apiv1.BanditArm{
			{Name: "control", GateLevel: 1},
			{Name: "treatment", GateLevel: 1},
		},
		DefaultGateLevel: 1,
	})
	// Get task and set its Next field to a non-existent gate
	task, _ := m.Task("implement")
	task.Next = "non-existent-gate"
	_, _, err := banditConfig(m, task)
	if err == nil {
		t.Fatal("expected task with non-existent gate to fail closed")
	}
	if !strings.Contains(err.Error(), "no such gate exists") {
		t.Fatalf("expected error about missing gate, got: %v", err)
	}
}

func TestBanditConfigAllowsArmsWithEqualOrHigherGateLevel(t *testing.T) {
	experiment := apiv1.BanditExperiment{
		Seed: 1,
		Arms: []apiv1.BanditArm{
			{Name: "control", GateLevel: 1},
			{Name: "treatment", GateLevel: 2},
		},
		DefaultGateLevel: 1,
	}
	m := experimentFixtureMachine(t, experiment)
	task, _ := m.Task("implement")
	_, _, err := banditConfig(m, task)
	if err != nil {
		t.Fatalf("valid experiment should succeed, got: %v", err)
	}
}

func TestBanditConfigRejectsArmsWithLowerGateLevel(t *testing.T) {
	experiment := apiv1.BanditExperiment{
		Seed: 1,
		Arms: []apiv1.BanditArm{
			{Name: "control", GateLevel: 1},
			{Name: "treatment", GateLevel: 0},
		},
		DefaultGateLevel: 1,
	}
	m := experimentFixtureMachine(t, experiment)
	task, _ := m.Task("implement")
	_, _, err := banditConfig(m, task)
	if err == nil {
		t.Fatal("expected arm with lower gate level to fail closed")
	}
	if !strings.Contains(err.Error(), "must not weaken the gate") {
		t.Fatalf("expected error about weakening the gate, got: %v", err)
	}
}

func TestBanditConfigRejectsControlArmMismatchedWithActualGate(t *testing.T) {
	m := experimentFixtureMachine(t, apiv1.BanditExperiment{
		Arms: []apiv1.BanditArm{
			{Name: "control", GateLevel: 2},
			{Name: "treatment", GateLevel: 2},
		},
		DefaultGateLevel: 2,
	})
	task, _ := m.Task("implement")
	_, _, err := banditConfig(m, task)
	if err == nil {
		t.Fatal("expected control arm mismatch with actual gate to fail closed")
	}
	if !strings.Contains(err.Error(), "must match actual gate") {
		t.Fatalf("expected error about actual gate level, got: %v", err)
	}
}

func TestRunnerExperimentAssignmentObservationAndPromotionAreRecorded(t *testing.T) {
	experiment := apiv1.BanditExperiment{
		Seed: 7,
		Arms: []apiv1.BanditArm{
			{Name: "control", GateLevel: 1, Variant: map[string]string{"harness": "control"}},
			{Name: "treatment", GateLevel: 1, Variant: map[string]string{"harness": "treatment"}},
		},
		ExplorationBudget: 100,
		MinSamples:        2,
		MaxFailureRate:    0.95,
		MinLift:           0.2,
		Confidence:        0.6,
		TrainWindow:       4,
		EvalWindow:        20,
		DefaultGateLevel:  1,
	}
	var deterministic *banditCapturingDeterministic
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		deterministic = &banditCapturingDeterministic{}
		return deterministic, nil
	}, gate.NewAutomatedEvaluator())
	seedObservations := seededBanditObservations("implement")
	writeBanditHistory(t, runsDir, seedObservations)

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-bandit",
		Machine: experimentFixtureMachine(t, experiment),
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-bandit"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var assignment, observation, proposal *journal.Event
	for i := range events {
		event := &events[i]
		switch event.Type {
		case journal.EventBanditAssignment:
			assignment = event
		case journal.EventBanditObservation:
			observation = event
		case journal.EventBanditPromotionProposed:
			proposal = event
		}
	}
	if assignment == nil || observation == nil || proposal == nil {
		t.Fatalf("missing expected bandit events: assignment=%t observation=%t proposal=%t", assignment != nil, observation != nil, proposal != nil)
	}
	if assignment.Stage != "implement" || observation.Stage != "implement" || proposal.Stage != "implement" {
		t.Fatalf("bandit events must be attributed to implement: assignment=%q observation=%q proposal=%q", assignment.Stage, observation.Stage, proposal.Stage)
	}

	variant, ok := assignment.Outputs["variant"].(map[string]any)
	if !ok {
		t.Fatalf("assignment variant = %#v, want object", assignment.Outputs["variant"])
	}
	harness, ok := variant["harness"].(string)
	if !ok || harness == "" {
		t.Fatalf("assignment variant harness = %#v", variant["harness"])
	}
	if deterministic.env.Inputs["harness"] != harness {
		t.Fatalf("task input harness = %q, want assigned variant %q", deterministic.env.Inputs["harness"], harness)
	}
	if observation.Outputs["schema"] != bandit.Schema || observation.Outputs["runId"] != "run-bandit" || observation.Outputs["window"] != "eval" {
		t.Fatalf("observation outputs = %+v, want schema/runId/window", observation.Outputs)
	}
	if requiresApproval, ok := proposal.Outputs["requiresApproval"].(bool); !ok || !requiresApproval {
		t.Fatalf("proposal requiresApproval = %#v, want true", proposal.Outputs["requiresApproval"])
	}
}

func TestRunnerAgenticExperimentRecordsObservationAfterInvocation(t *testing.T) {
	experiment := apiv1.BanditExperiment{
		Seed: 7,
		Arms: []apiv1.BanditArm{
			{Name: "control", GateLevel: 1, Variant: map[string]string{"harness": "control"}},
			{Name: "treatment", GateLevel: 1, Variant: map[string]string{"harness": "treatment"}},
		},
		ExplorationBudget: 100,
		MinSamples:        2,
		MaxFailureRate:    0.95,
		MinLift:           0.2,
		Confidence:        0.6,
		TrainWindow:       4,
		EvalWindow:        20,
		DefaultGateLevel:  1,
	}
	runsDir, fixtureRepo, worktrees := newTestRunnerEnv(t)
	goober := &banditCapturingGoober{}
	r, err := New(Config{
		NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
			return goober, nil
		},
		Automated: gate.NewAutomatedEvaluator(),
		Worktrees: worktrees,
		RunsDir:   runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			return fixtureRepo, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	writeBanditHistory(t, runsDir, seededBanditObservations("implement"))

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-bandit-agentic",
		Machine: experimentFixtureMachine(t, experiment, apiv1.TaskAgentic),
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	if goober.env.Inputs["harness"] == "" {
		t.Fatal("agentic invocation did not receive assigned variant")
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-bandit-agentic"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	for _, event := range events {
		if event.Type == journal.EventBanditObservation && event.Stage == "implement" {
			return
		}
	}
	t.Fatal("agentic experiment did not record a bandit observation")
}

func experimentFixtureMachine(t *testing.T, experiment apiv1.BanditExperiment, taskTypes ...apiv1.TaskType) *workflow.Machine {
	t.Helper()
	taskType := apiv1.TaskDeterministic
	if len(taskTypes) > 0 {
		taskType = taskTypes[0]
	}
	task := apiv1.Task{
		Name: "implement", Type: taskType, Goal: "produce a diff",
		Inputs: map[string]string{
			"harness": "baseline",
		},
		Experiment: &experiment,
		Next:       "review",
	}
	if taskType == apiv1.TaskDeterministic {
		task.Run = &apiv1.DeterministicRun{Command: []string{"true"}}
	} else {
		task.Goober = "implementer"
	}
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks:    []apiv1.Task{task},
		Gates: []apiv1.Gate{
			{
				Name:      "review",
				Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "status-equals"},
				Branches: map[string]string{
					"pass": workflow.TerminalComplete,
					"fail": workflow.TargetAbort,
				},
			},
		},
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "bandit-fixture", Version: 1, Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile machine: %v", err)
	}
	return machine
}

func seededBanditObservations(stage string) []bandit.Observation {
	var observations []bandit.Observation
	for i := 0; i < 2; i++ {
		observations = append(observations,
			bandit.Observation{Stage: stage, RunID: fmt.Sprintf("train-control-%d", i), Arm: "control", Success: false, RewardSet: true, Window: "train", Assigned: uint64(i + 1)},
			bandit.Observation{Stage: stage, RunID: fmt.Sprintf("train-treatment-%d", i), Arm: "treatment", Success: true, RewardSet: true, Reward: 1, Window: "train", Assigned: uint64(i + 3)},
		)
	}
	for i := 0; i < 10; i++ {
		observations = append(observations,
			bandit.Observation{Stage: stage, RunID: fmt.Sprintf("eval-control-%d", i), Arm: "control", Success: false, RewardSet: true, Window: "eval", Assigned: uint64(i + 100)},
			bandit.Observation{Stage: stage, RunID: fmt.Sprintf("eval-treatment-%d", i), Arm: "treatment", Success: true, RewardSet: true, Reward: 1, Window: "eval", Assigned: uint64(i + 200)},
		)
	}
	return observations
}

func writeBanditHistory(t *testing.T, runsDir string, observations []bandit.Observation) {
	t.Helper()
	if len(observations) == 0 {
		return
	}
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "history-bandit", Workflow: "fixture", WorkflowVersion: 1,
		WorkflowDigest: "sha256:fixture", GooberDigest: "sha256:fixture",
		Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("create history journal: %v", err)
	}
	cfg := bandit.Config{Stage: observations[0].Stage}
	for _, observation := range observations {
		if err := cfg.RecordObservation(observation, run); err != nil {
			t.Fatalf("record history observation: %v", err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close history journal: %v", err)
	}
}
