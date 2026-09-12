package workflow

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestCheckProviderStageInputsRejectsRetiredLiteralAndDynamicInputs(t *testing.T) {
	for _, test := range []struct {
		name       string
		inputs     map[string]string
		inputsFrom map[string]string
		experiment *apiv1.BanditExperiment
	}{
		{name: "literal", inputs: map[string]string{"resweepInterval": "6h"}},
		{name: "dynamic", inputsFrom: map[string]string{"resweepInterval": "interval"}},
		{name: "experiment arm", experiment: &apiv1.BanditExperiment{Arms: []apiv1.BanditArm{{
			Name: "legacy", Variant: map[string]string{"resweepInterval": "6h"},
		}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			def := Definition{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{{
				Name:       "query",
				Type:       apiv1.TaskDeterministic,
				Run:        &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query", "--claim"}},
				Inputs:     test.inputs,
				InputsFrom: test.inputsFrom,
				Experiment: test.experiment,
			}}}}
			problems := CheckProviderStageInputs(def)
			if len(problems) != 1 || !strings.Contains(problems[0], `retired input "resweepInterval"`) ||
				!strings.Contains(problems[0], "separate workflow") ||
				!strings.Contains(problems[0], "backlog-query --claim --resweep") {
				t.Fatalf("problems = %q, want one actionable retirement error", problems)
			}
			if test.experiment != nil && !strings.Contains(problems[0], `experiment arm "legacy"`) {
				t.Fatalf("experiment problem = %q, want arm attribution", problems[0])
			}
		})
	}
}

func TestCheckProviderStageInputsAllowsCurrentAndUnknownInputs(t *testing.T) {
	def := Definition{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{
		{
			Name:   "query",
			Type:   apiv1.TaskDeterministic,
			Run:    &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query", "--claim", "--resweep"}},
			Inputs: map[string]string{"resweepMaxItems": "5", "resweepReadyLabel": "ready", "extensionInput": "allowed"},
		},
		{
			Name:   "external",
			Type:   apiv1.TaskDeterministic,
			Run:    &apiv1.DeterministicRun{Command: []string{"other", "backlog-query"}},
			Inputs: map[string]string{"resweepInterval": "6h"},
		},
	}}}
	if problems := CheckProviderStageInputs(def); len(problems) != 0 {
		t.Fatalf("problems = %q, want current and open-schema inputs accepted", problems)
	}
}
