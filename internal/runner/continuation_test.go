package runner

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

func continuationMachine(t *testing.T, next string) *workflow.Machine {
	t.Helper()
	machine, err := workflow.Compile(workflow.Definition{
		Name: "continuation", Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "g", Start: "start",
			Tasks: []apiv1.Task{
				{Name: "infra", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: next},
				{Name: "finish", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: []string{"true"}}},
			},
			Gates: []apiv1.Gate{{
				Name: "start", Evaluator: apiv1.EvaluatorAutomated,
				Branches: map[string]string{"pass": "infra", "fail": "finish"},
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	return machine
}

func TestValidateContinuationTargetAdmitsInfrastructureStage(t *testing.T) {
	machine := continuationMachine(t, "finish")
	if err := ValidateContinuationTarget(machine, machine, "infra"); err != nil {
		t.Fatalf("ValidateContinuationTarget: %v", err)
	}
}

func TestValidateContinuationTargetRejectsMissingAndChangedTargets(t *testing.T) {
	source := continuationMachine(t, "finish")
	candidate := continuationMachine(t, workflow.TerminalComplete)
	for _, target := range []string{"missing", workflow.TargetAbort, workflow.TerminalComplete, "infra"} {
		err := ValidateContinuationTarget(source, candidate, target)
		if err == nil {
			t.Fatalf("target %q was admitted", target)
		}
		for _, value := range []string{target, source.Digest(), candidate.Digest()} {
			if !strings.Contains(err.Error(), value) {
				t.Fatalf("error %q does not contain %q", err, value)
			}
		}
	}
}

func TestValidateContinuationTargetRejectsChangedExecutionSemantics(t *testing.T) {
	source := continuationMachineWithMetadata(t, "source-owner", apiv1.EvaluatorAutomated)
	candidate := continuationMachineWithMetadata(t, "candidate-owner", apiv1.EvaluatorAutomated)

	err := ValidateContinuationTarget(source, candidate, "infra")
	if err == nil {
		t.Fatal("target with changed execution semantics was admitted")
	}

	for _, value := range []string{"infra", source.Digest(), candidate.Digest()} {
		if !strings.Contains(err.Error(), value) {
			t.Fatalf("error %q does not contain %q", err, value)
		}
	}
}

func TestValidateContinuationTargetRejectsChangedDeterministicExecution(t *testing.T) {
	source := continuationMachine(t, "finish")
	candidate, err := workflow.Compile(workflow.Definition{
		Name: "continuation", Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "g", Start: "start",
			Tasks: []apiv1.Task{
				{Name: "infra", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{
					Command: []string{"different-command"}, Workspace: apiv1.WorkspaceScratch,
				}, Next: "finish"},
				{Name: "finish", Type: apiv1.TaskDeterministic,
					Run: &apiv1.DeterministicRun{Command: []string{"true"}}},
			},
			Gates: []apiv1.Gate{{
				Name: "start", Evaluator: apiv1.EvaluatorAutomated,
				Branches: map[string]string{"pass": "infra", "fail": "finish"},
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}

	if err := ValidateContinuationTarget(source, candidate, "infra"); err == nil {
		t.Fatal("target with changed deterministic execution was admitted")
	}
}

func continuationMachineWithMetadata(t *testing.T, owner string, evaluator apiv1.EvaluatorKind) *workflow.Machine {
	t.Helper()
	machine, err := workflow.Compile(workflow.Definition{
		Name: "continuation", Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "g", Start: "start",
			Tasks: []apiv1.Task{{
				Name: "infra", Type: apiv1.TaskAgentic, Goober: owner, Next: "finish",
			}, {
				Name: "finish", Type: apiv1.TaskDeterministic,
				Run: &apiv1.DeterministicRun{Command: []string{"true"}},
			}},
			Gates: []apiv1.Gate{{
				Name: "start", Evaluator: evaluator,
				Branches: map[string]string{"pass": "infra", "fail": "finish"},
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	return machine
}

func TestValidateContinuationTargetUsesHistoricalSourceDigest(t *testing.T) {
	runsDir := t.TempDir()
	source := continuationMachine(t, "finish")
	candidate := continuationMachine(t, workflow.TerminalComplete)
	newPinnedDefinitionRun(t, runsDir, "historical-source", source)

	reader, err := journal.OpenRead(filepath.Join(runsDir, "historical-source"))
	if err != nil {
		t.Fatal(err)
	}
	identity, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	reconstructed, err := PinnedWorkflowMachine(reader, identity)
	if err != nil {
		t.Fatalf("PinnedWorkflowMachine: %v", err)
	}
	if err := ValidateContinuationTarget(reconstructed, candidate, "infra"); err == nil {
		t.Fatal("incompatible historical continuation was admitted")
	} else {
		for _, value := range []string{"infra", source.Digest(), candidate.Digest()} {
			if !strings.Contains(err.Error(), value) {
				t.Fatalf("error %q does not contain %q", err, value)
			}
		}
	}
}

func TestResumeFreshContinuationStartsAtRequestedTarget(t *testing.T) {
	machine, err := workflow.Compile(workflow.Definition{
		Name: "continuation", Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "acme-web", Start: "prepare",
			Tasks: []apiv1.Task{
				{
					Name: "prepare", Type: apiv1.TaskDeterministic,
					Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "finish",
				},
				{
					Name: "finish", Type: apiv1.TaskDeterministic,
					Run: &apiv1.DeterministicRun{Command: []string{"true"}},
				},
			},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}

	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)
	definition, err := json.Marshal(machine.Def)
	if err != nil {
		t.Fatal(err)
	}
	source, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "source-run", Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, map[string][]byte{
		journal.PinnedWorkflowDefinitionInputName: definition,
	}, journal.WithInputIntegrity(map[string]apiv1.Integrity{
		journal.PinnedWorkflowDefinitionInputName: apiv1.IntegrityTrusted,
	}))
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	if err := source.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	sourceReader, err := journal.OpenRead(filepath.Join(runsDir, "source-run"))
	if err != nil {
		t.Fatal(err)
	}
	sourceEvents, err := sourceReader.Events()
	if err != nil {
		t.Fatal(err)
	}
	continuation, err := journal.CreateContinuation(runsDir, journal.ContinuationRequest{
		RunID: "continued-run", SourceRunID: "source-run",
		ExpectedTerminalSeq: sourceEvents[len(sourceEvents)-1].Seq,
		Operator:            "operator@example.test",
		Target:              "finish",
	})
	if err != nil {
		t.Fatalf("CreateContinuation: %v", err)
	}
	if err := continuation.Close(); err != nil {
		t.Fatal(err)
	}

	counting := &countingDeterministic{}
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return counting, nil },
		Automated:        gate.NewAutomatedEvaluator(),
		Worktrees:        wtMgr,
		RunsDir:          runsDir,
		RepoCloneURL:     func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.Resume(context.Background(), ResumeInput{
		RunID: "continued-run", Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("result = %+v, want completed", result)
	}
	if counting.calls != 1 {
		t.Fatalf("deterministic calls = %d, want only the requested target stage", counting.calls)
	}
	reader, err := journal.OpenRead(filepath.Join(runsDir, "continued-run"))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	started := 0
	for _, event := range events {
		if event.Type == journal.EventStageStarted {
			started++
			if event.Stage != "finish" {
				t.Fatalf("stage.started = %+v, want only finish", event)
			}
		}
	}
	if started != 1 {
		t.Fatalf("stage.started count = %d, want 1", started)
	}
}
