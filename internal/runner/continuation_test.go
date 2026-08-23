package runner

import (
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
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
