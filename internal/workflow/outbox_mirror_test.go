package workflow

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestCompileResolvesWorkflowOutboxMirrorWithTaskPrecedence(t *testing.T) {
	def := Definition{
		Name:    "outbox-mirror",
		Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle:           "example",
			Triggers:         []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:            "first",
			OutboxMirrorPath: "/workflow",
			Tasks: []apiv1.Task{
				{Name: "first", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "second"},
				{Name: "second", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: []string{"true"}}, OutboxMirrorPath: "/task"},
			},
		},
	}

	machine, err := Compile(def, WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	first, _ := machine.Task("first")
	second, _ := machine.Task("second")
	if first.OutboxMirrorPath != "/workflow" {
		t.Fatalf("first mirror = %q, want /workflow", first.OutboxMirrorPath)
	}
	if second.OutboxMirrorPath != "/task" {
		t.Fatalf("second mirror = %q, want /task", second.OutboxMirrorPath)
	}
	if def.Spec.Tasks[0].OutboxMirrorPath != "" {
		t.Fatal("Compile mutated the caller's definition")
	}
}
