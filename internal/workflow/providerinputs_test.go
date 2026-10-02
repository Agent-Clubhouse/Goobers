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

func TestCheckProviderStageInputsAllowsCurrentAndExternalInputs(t *testing.T) {
	def := Definition{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{
		{
			Name:   "query",
			Type:   apiv1.TaskDeterministic,
			Run:    &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query", "--claim", "--resweep"}},
			Inputs: map[string]string{"resweepMaxItems": "5", "resweepReadyLabel": "ready"},
		},
		{
			Name:   "external",
			Type:   apiv1.TaskDeterministic,
			Run:    &apiv1.DeterministicRun{Command: []string{"other", "backlog-query"}},
			Inputs: map[string]string{"resweepInterval": "6h"},
		},
	}}}
	if problems := CheckProviderStageInputs(def); len(problems) != 0 {
		t.Fatalf("problems = %q, want current built-in and external inputs accepted", problems)
	}
}

func TestCheckProviderStageInputsAllowsExecutorWideInputsForEveryBuiltIn(t *testing.T) {
	def := Definition{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{
		{
			Name: "update",
			Type: apiv1.TaskDeterministic,
			Run:  &apiv1.DeterministicRun{Command: []string{"goobers", "self-update"}},
			Inputs: map[string]string{
				"timeout":        "5m",
				"maxOutputBytes": "1048576",
			},
		},
	}}}
	if problems := CheckProviderStageInputs(def); len(problems) != 0 {
		t.Fatalf("problems = %q, want executor-wide shell inputs accepted for self-update", problems)
	}
}

func TestCheckProviderStageInputsRejectsUndeclaredBuiltInInput(t *testing.T) {
	def := Definition{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{{
		Name:   "query",
		Type:   apiv1.TaskDeterministic,
		Run:    &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query"}},
		Inputs: map[string]string{"extensionInput": "not-a-backlog-query-input"},
	}}}}
	problems := CheckProviderStageInputs(def)
	if len(problems) != 1 || !strings.Contains(problems[0], `undeclared input "extensionInput"`) {
		t.Fatalf("problems = %q, want undeclared built-in input refusal", problems)
	}
}

func remediationCheckpointTask(inputs, inputsFrom map[string]string, experiment *apiv1.BanditExperiment) apiv1.Task {
	return apiv1.Task{
		Name:       "remediation-checkpoint",
		Type:       apiv1.TaskDeterministic,
		Run:        &apiv1.DeterministicRun{Command: []string{"goobers", "remediation-checkpoint"}},
		Inputs:     inputs,
		InputsFrom: inputsFrom,
		Experiment: experiment,
	}
}

// TestCheckProviderStageUnsetDefaultsWarnsPerUnsetBudget pins #2737: every
// remediation-checkpoint per-cause budget the task leaves unset is reported
// (the stage defaults it to 2), and only those.
func TestCheckProviderStageUnsetDefaultsWarnsPerUnsetBudget(t *testing.T) {
	def := Definition{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{remediationCheckpointTask(
		map[string]string{"conflictBudget": "3", "humanCommentBudget": "2"},
		map[string]string{"substantiveBudget": "budget"},
		nil,
	)}}}
	problems := CheckProviderStageUnsetDefaults(def)
	if len(problems) != 2 {
		t.Fatalf("problems = %q, want exactly failingCIBudget and siblingOverlapBudget", problems)
	}
	joined := strings.Join(problems, "\n")
	for _, want := range []string{`"failingCIBudget"`, `"siblingOverlapBudget"`, "defaults to 2", "goobers remediation-checkpoint"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("problems = %q, want mention of %s", problems, want)
		}
	}
}

func TestCheckProviderStageUnsetDefaultsQuietWhenEverythingSet(t *testing.T) {
	all := map[string]string{
		"conflictBudget": "2", "substantiveBudget": "2", "failingCIBudget": "2",
		"siblingOverlapBudget": "2", "humanCommentBudget": "2",
	}
	external := apiv1.Task{
		Name: "external",
		Type: apiv1.TaskDeterministic,
		Run:  &apiv1.DeterministicRun{Command: []string{"other", "remediation-checkpoint"}},
	}
	def := Definition{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{remediationCheckpointTask(all, nil, nil), external}}}
	if problems := CheckProviderStageUnsetDefaults(def); len(problems) != 0 {
		t.Fatalf("problems = %q, want no warnings when every budget is set and for external commands", problems)
	}
}

func TestCheckProviderStageUnsetDefaultsEmptyLiteralStillWarns(t *testing.T) {
	inputs := map[string]string{
		"conflictBudget": "", "substantiveBudget": "2", "failingCIBudget": "2",
		"siblingOverlapBudget": "2", "humanCommentBudget": "2",
	}
	def := Definition{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{remediationCheckpointTask(inputs, nil, nil)}}}
	problems := CheckProviderStageUnsetDefaults(def)
	if len(problems) != 1 || !strings.Contains(problems[0], `"conflictBudget"`) {
		t.Fatalf("problems = %q, want conflictBudget warning for an empty literal that runs on the default", problems)
	}
}

func TestCheckProviderStageUnsetDefaultsQuietWhenOverrideFlagPresent(t *testing.T) {
	for _, command := range [][]string{
		{"goobers", "remediation-checkpoint", "--budget", "3"},
		{"goobers", "remediation-checkpoint", "--budget=3"},
	} {
		task := remediationCheckpointTask(nil, nil, nil)
		task.Run = &apiv1.DeterministicRun{Command: command}
		def := Definition{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{task}}}
		if problems := CheckProviderStageUnsetDefaults(def); len(problems) != 0 {
			t.Fatalf("command %q: problems = %q, want none when --budget supersedes every default", command, problems)
		}
	}
}

func TestCheckProviderStageUnsetDefaultsExperimentArmsMustAllSetInput(t *testing.T) {
	base := map[string]string{"conflictBudget": "2", "substantiveBudget": "2", "failingCIBudget": "2", "humanCommentBudget": "2"}
	everyArm := &apiv1.BanditExperiment{Arms: []apiv1.BanditArm{
		{Name: "tight", Variant: map[string]string{"siblingOverlapBudget": "1"}},
		{Name: "loose", Variant: map[string]string{"siblingOverlapBudget": "4"}},
	}}
	def := Definition{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{remediationCheckpointTask(base, nil, everyArm)}}}
	if problems := CheckProviderStageUnsetDefaults(def); len(problems) != 0 {
		t.Fatalf("problems = %q, want no warning when every arm sets the budget", problems)
	}

	oneArm := &apiv1.BanditExperiment{Arms: []apiv1.BanditArm{
		{Name: "tight", Variant: map[string]string{"siblingOverlapBudget": "1"}},
		{Name: "control"},
	}}
	def = Definition{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{remediationCheckpointTask(base, nil, oneArm)}}}
	if problems := CheckProviderStageUnsetDefaults(def); len(problems) != 1 || !strings.Contains(problems[0], `"siblingOverlapBudget"`) {
		t.Fatalf("problems = %q, want siblingOverlapBudget warning when an arm runs on the default", problems)
	}
}
