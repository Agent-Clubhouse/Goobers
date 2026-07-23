package vcurrent

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestTaskInvocationInputsUsesDownstreamGatePollCadence(t *testing.T) {
	task := apiv1.Task{
		Name:   "ci-poll",
		Inputs: map[string]string{taskKindInput: ciPollTaskKind, ciPollIntervalInput: "30s"},
		Next:   "ci-gate",
	}
	machine := newMachine(Definition{Spec: apiv1.WorkflowSpec{
		Tasks: []apiv1.Task{task},
		Gates: []apiv1.Gate{{
			Name:      "ci-gate",
			Evaluator: apiv1.EvaluatorAutomated,
			Automated: &apiv1.AutomatedGate{Check: "ci-status", PollIntervalSeconds: 7},
		}},
	}})

	got := TaskInvocationInputs(machine, task)

	if got[ciPollIntervalInput] != "7s" {
		t.Fatalf("poll interval input = %q, want downstream gate cadence 7s", got[ciPollIntervalInput])
	}
	if task.Inputs[ciPollIntervalInput] != "30s" {
		t.Fatalf("TaskInvocationInputs mutated task inputs: %q", task.Inputs[ciPollIntervalInput])
	}
}

func TestTaskInvocationInputsPreservesLegacyCIPollCadenceWithoutGatePolicy(t *testing.T) {
	task := apiv1.Task{
		Inputs: map[string]string{taskKindInput: ciPollTaskKind, ciPollIntervalInput: "30s"},
	}
	machine := newMachine(Definition{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{task}}})

	got := TaskInvocationInputs(machine, task)

	if got[ciPollIntervalInput] != "30s" {
		t.Fatalf("poll interval input = %q, want legacy task input 30s", got[ciPollIntervalInput])
	}
}
