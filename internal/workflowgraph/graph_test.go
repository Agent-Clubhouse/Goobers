package workflowgraph

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow"
)

func TestBranchContainsState(t *testing.T) {
	machine, err := workflow.Compile(workflow.Definition{
		Name: "branch-contains-state", Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "test", Start: "first",
			Tasks: []apiv1.Task{
				{Name: "first", Type: apiv1.TaskDeterministic, Goal: "first", Run: testRun(), Next: "choice"},
				{Name: "cycle", Type: apiv1.TaskDeterministic, Goal: "cycle", Run: testRun(), Next: "choice"},
				{Name: "terminal", Type: apiv1.TaskDeterministic, Goal: "terminal", Run: testRun(), Next: workflow.TargetAbort},
			},
			Gates: []apiv1.Gate{{
				Name:      "choice",
				Evaluator: apiv1.EvaluatorAgentic,
				Agentic:   &apiv1.AgenticGate{Goober: "reviewer", Workspace: apiv1.WorkspaceScratch},
				Branches: map[string]string{
					"pass":          "cycle",
					"fail":          "terminal",
					"needs-changes": "terminal",
				},
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		start  string
		target string
		want   bool
	}{
		{name: "successful branch path", start: "first", target: "cycle", want: true},
		{name: "cycle reaches exit", start: "cycle", target: "terminal", want: true},
		{name: "missing start", start: "missing", target: "cycle", want: false},
		{name: "missing target", start: "first", target: "missing", want: false},
		{name: "empty start", start: "", target: "first", want: false},
		{name: "reachable reserved target", start: "terminal", target: workflow.TargetAbort, want: true},
		{name: "reserved start does not traverse", start: workflow.TargetAbort, target: "terminal", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := BranchContainsState(machine, test.start, test.target); got != test.want {
				t.Fatalf("BranchContainsState(%q, %q) = %v, want %v", test.start, test.target, got, test.want)
			}
		})
	}
}

func testRun() *apiv1.DeterministicRun {
	return &apiv1.DeterministicRun{
		Command:   []string{"true"},
		Workspace: apiv1.WorkspaceScratch,
	}
}
