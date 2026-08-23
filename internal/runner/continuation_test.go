package runner

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
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
