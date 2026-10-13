package workflow

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestCheckStageDurationInputsRejectsUnparseableStaticDurations(t *testing.T) {
	for _, test := range []struct {
		name       string
		command    []string
		inputs     map[string]string
		inputsFrom map[string]string
		timeout    int32
		experiment *apiv1.BanditExperiment
		want       []string
	}{
		{
			// A dynamically resolved kind may still select the shell executor,
			// which runs the built-in that parses leaseDuration.
			name:       "built-in duration with dynamic kind",
			command:    []string{"goobers", "pr-claim"},
			inputs:     map[string]string{"leaseDuration": "3300"},
			inputsFrom: map[string]string{"kind": "plan.kind"},
			timeout:    3600,
			want:       []string{`task "stage" inputs.leaseDuration "3300"`, `"3300s" (55m0s)`},
		},
		{
			name:       "ci-poll duration with dynamic kind",
			command:    []string{"watch-queue"},
			inputs:     map[string]string{"pollTimeoutSeconds": "3300"},
			inputsFrom: map[string]string{"kind": "plan.kind"},
			timeout:    3600,
			want:       []string{`task "stage" inputs.pollTimeoutSeconds "3300"`},
		},
		{
			name:    "ci-poll bare integer timeout",
			command: []string{"goobers", "ci-poll"},
			inputs:  map[string]string{"kind": "ci-poll", "pollTimeoutSeconds": "3300"},
			timeout: 3600,
			want:    []string{`task "stage" inputs.pollTimeoutSeconds "3300" is not a valid duration`, `"3300s" (55m0s)`},
		},
		{
			name:    "ci-poll max interval",
			command: []string{"goobers", "ci-poll"},
			inputs:  map[string]string{"kind": "ci-poll", "pollMaxIntervalSeconds": "two minutes"},
			want:    []string{`inputs.pollMaxIntervalSeconds "two minutes"`, `such as "90s"`},
		},
		{
			name:    "ci-poll retry backoff",
			command: []string{"goobers", "ci-poll"},
			inputs:  map[string]string{"kind": "ci-poll", "retryFailedChecksBackoffSeconds": "60"},
			want:    []string{`inputs.retryFailedChecksBackoffSeconds "60"`, `"60s" (1m0s)`},
		},
		{
			name:    "shell timeout without task ceiling",
			command: []string{"make", "ci"},
			inputs:  map[string]string{"timeout": "600"},
			want:    []string{`inputs.timeout "600"`, `"600s" (10m0s)`},
		},
		{
			// Gate cadence may replace the value at dispatch, but a static
			// duration-typed input must still parse.
			name:    "ci-poll interval replaced by gate cadence",
			command: []string{"goobers", "ci-poll"},
			inputs:  map[string]string{"kind": "ci-poll", "pollIntervalSeconds": "15"},
			want:    []string{`task "stage" inputs.pollIntervalSeconds "15"`, `"15s" (15s)`},
		},
		{
			name:       "ci-poll interval with dynamic kind",
			command:    []string{"goobers", "merge-queue-poll"},
			inputs:     map[string]string{"kind": "ci-poll", "pollIntervalSeconds": "30"},
			inputsFrom: map[string]string{"kind": "plan.kind"},
			timeout:    3600,
			want:       []string{`task "stage" inputs.pollIntervalSeconds "30"`},
		},
		{
			// A task ceiling takes precedence over inputs.timeout at run
			// time, but the static value must still parse.
			name:    "shell timeout behind task ceiling",
			command: []string{"make", "ci"},
			inputs:  map[string]string{"timeout": "600"},
			timeout: 600,
			want:    []string{`inputs.timeout "600"`, `"600s" (10m0s)`},
		},
		{
			name:    "built-in provider duration",
			command: []string{"goobers", "merge-queue-poll"},
			inputs:  map[string]string{"pollTimeoutSeconds": "1800"},
			timeout: 3600,
			want:    []string{`inputs.pollTimeoutSeconds "1800"`},
		},
		{
			name:    "experiment arm variant",
			command: []string{"goobers", "ci-poll"},
			inputs:  map[string]string{"kind": "ci-poll", "pollTimeoutSeconds": "30m"},
			experiment: &apiv1.BanditExperiment{Arms: []apiv1.BanditArm{{
				Name: "long", Variant: map[string]string{"pollTimeoutSeconds": "3300"},
			}}},
			want: []string{`task "stage" experiment arm "long" input pollTimeoutSeconds "3300"`},
		},
		{
			// The runner applies variants after replacing the base cadence
			// with the downstream gate's, so a variant interval is parsed.
			name:    "experiment arm ci-poll interval",
			command: []string{"goobers", "ci-poll"},
			inputs:  map[string]string{"kind": "ci-poll"},
			experiment: &apiv1.BanditExperiment{Arms: []apiv1.BanditArm{{
				Name: "fast", Variant: map[string]string{"pollIntervalSeconds": "5"},
			}}},
			want: []string{`experiment arm "fast" input pollIntervalSeconds "5"`},
		},
		{
			name:    "experiment arm switches kind to ci-poll",
			command: []string{"watch-queue"},
			inputs:  map[string]string{"pollTimeoutSeconds": "3300"},
			timeout: 3600,
			experiment: &apiv1.BanditExperiment{Arms: []apiv1.BanditArm{{
				Name: "poll", Variant: map[string]string{"kind": "ci-poll"},
			}}},
			want: []string{`task "stage" inputs.pollTimeoutSeconds "3300"`},
		},
		{
			name:    "base value passed through by several arms is reported once",
			command: []string{"make", "ci"},
			inputs:  map[string]string{"timeout": "600"},
			experiment: &apiv1.BanditExperiment{Arms: []apiv1.BanditArm{
				{Name: "a", Variant: map[string]string{"other": "x"}},
				{Name: "b", Variant: map[string]string{"other": "y"}},
			}},
			want: []string{`task "stage" inputs.timeout "600"`},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			def := Definition{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{{
				Name:           "stage",
				Type:           apiv1.TaskDeterministic,
				Run:            &apiv1.DeterministicRun{Command: test.command},
				Inputs:         test.inputs,
				InputsFrom:     test.inputsFrom,
				TimeoutSeconds: test.timeout,
				Experiment:     test.experiment,
			}}}}
			problems := CheckStageDurationInputs(def)
			if len(problems) != 1 {
				t.Fatalf("problems = %q, want exactly one", problems)
			}
			for _, want := range test.want {
				if !strings.Contains(problems[0], want) {
					t.Fatalf("problem = %q, want it to contain %q", problems[0], want)
				}
			}
		})
	}
}

func TestCheckStageDurationInputsIgnoresValuesTheRuntimeNeverParses(t *testing.T) {
	def := Definition{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{
		{
			Name: "valid",
			Type: apiv1.TaskDeterministic,
			Run:  &apiv1.DeterministicRun{Command: []string{"goobers", "ci-poll"}},
			Inputs: map[string]string{
				"kind": "ci-poll", "pollTimeoutSeconds": "55m", "pollMaxIntervalSeconds": "",
			},
		},
		{
			Name:       "dynamic",
			Type:       apiv1.TaskDeterministic,
			Run:        &apiv1.DeterministicRun{Command: []string{"goobers", "ci-poll"}},
			Inputs:     map[string]string{"kind": "ci-poll", "pollTimeoutSeconds": "3300"},
			InputsFrom: map[string]string{"pollTimeoutSeconds": "plan.pollBudget"},
			Experiment: &apiv1.BanditExperiment{Arms: []apiv1.BanditArm{{
				Name: "long", Variant: map[string]string{"pollTimeoutSeconds": "3300"},
			}}},
		},
		{
			// Every run takes an arm, and every arm replaces the base value.
			Name:   "arms-override-timeout",
			Type:   apiv1.TaskDeterministic,
			Run:    &apiv1.DeterministicRun{Command: []string{"make", "ci"}},
			Inputs: map[string]string{"timeout": "600"},
			Experiment: &apiv1.BanditExperiment{Arms: []apiv1.BanditArm{
				{Name: "fast", Variant: map[string]string{"timeout": "5m"}},
				{Name: "poll", Variant: map[string]string{"kind": "ci-poll"}},
			}},
		},
		{
			// With a dynamic kind, a value inputsFrom supplies individually
			// is still left to the runtime parser.
			Name:           "dynamic-kind-dynamic-duration",
			Type:           apiv1.TaskDeterministic,
			Run:            &apiv1.DeterministicRun{Command: []string{"goobers", "pr-claim"}},
			Inputs:         map[string]string{"leaseDuration": "3300"},
			InputsFrom:     map[string]string{"kind": "plan.kind", "leaseDuration": "plan.lease"},
			TimeoutSeconds: 3600,
		},
		{
			// External commands own their inputs; only the shell executor's
			// timeout is parsed on their behalf.
			Name:   "external",
			Type:   apiv1.TaskDeterministic,
			Run:    &apiv1.DeterministicRun{Command: []string{"watch-queue"}},
			Inputs: map[string]string{"pollTimeoutSeconds": "3300", "timeout": "15m"},
		},
		{
			Name:   "agentic",
			Type:   apiv1.TaskAgentic,
			Inputs: map[string]string{"timeout": "600"},
		},
	}}}
	if problems := CheckStageDurationInputs(def); len(problems) != 0 {
		t.Fatalf("problems = %q, want none", problems)
	}
}

func TestCheckStageDurationInputsRejectsExternalTelemetryDurations(t *testing.T) {
	telemetryTask := func(inputs, inputsFrom map[string]string, experiment *apiv1.BanditExperiment) Definition {
		base := map[string]string{"kind": "external-telemetry", "connector": "metrics", "query": "health"}
		for key, value := range inputs {
			base[key] = value
		}
		return Definition{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{{
			Name:       "stage",
			Type:       apiv1.TaskDeterministic,
			Run:        &apiv1.DeterministicRun{Command: []string{"goobers", "external-telemetry"}},
			Inputs:     base,
			InputsFrom: inputsFrom,
			Experiment: experiment,
		}}}}
	}
	for _, name := range []string{"window", "freshness", "queryTimeout", "queryRetryBackoff"} {
		t.Run(name+" unparseable", func(t *testing.T) {
			problems := CheckStageDurationInputs(telemetryTask(map[string]string{name: "300"}, nil, nil))
			if len(problems) != 1 || !strings.Contains(problems[0], `inputs.`+name+` "300" is not a valid duration`) {
				t.Fatalf("problems = %q, want one unparseable %s error", problems, name)
			}
		})
		t.Run(name+" non-positive", func(t *testing.T) {
			problems := CheckStageDurationInputs(telemetryTask(map[string]string{name: "0s"}, nil, nil))
			if len(problems) != 1 || !strings.Contains(problems[0], `inputs.`+name+` "0s" must be a positive duration`) {
				t.Fatalf("problems = %q, want one non-positive %s error", problems, name)
			}
		})
		t.Run(name+" valid", func(t *testing.T) {
			if problems := CheckStageDurationInputs(telemetryTask(map[string]string{name: "5m"}, nil, nil)); len(problems) != 0 {
				t.Fatalf("problems = %q, want none", problems)
			}
		})
		t.Run(name+" from inputsFrom", func(t *testing.T) {
			def := telemetryTask(map[string]string{name: "300"}, map[string]string{name: "plan.value"}, nil)
			if problems := CheckStageDurationInputs(def); len(problems) != 0 {
				t.Fatalf("problems = %q, want inputsFrom value left to the runtime", problems)
			}
		})
	}
	t.Run("experiment arm variant", func(t *testing.T) {
		def := telemetryTask(map[string]string{"window": "1h"}, nil, &apiv1.BanditExperiment{Arms: []apiv1.BanditArm{{
			Name: "wide", Variant: map[string]string{"window": "24"},
		}}})
		problems := CheckStageDurationInputs(def)
		if len(problems) != 1 || !strings.Contains(problems[0], `experiment arm "wide" input window "24"`) {
			t.Fatalf("problems = %q, want one arm-attributed window error", problems)
		}
	})
	t.Run("arm switches kind to external-telemetry", func(t *testing.T) {
		def := Definition{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{{
			Name:           "stage",
			Type:           apiv1.TaskDeterministic,
			Run:            &apiv1.DeterministicRun{Command: []string{"watch-queue"}},
			Inputs:         map[string]string{"freshness": "900"},
			TimeoutSeconds: 3600,
			Experiment: &apiv1.BanditExperiment{Arms: []apiv1.BanditArm{{
				Name: "telemetry", Variant: map[string]string{"kind": "external-telemetry"},
			}}},
		}}}}
		problems := CheckStageDurationInputs(def)
		if len(problems) != 1 || !strings.Contains(problems[0], `inputs.freshness "900"`) {
			t.Fatalf("problems = %q, want one freshness error", problems)
		}
	})
	t.Run("dynamic kind", func(t *testing.T) {
		def := Definition{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{{
			Name:           "stage",
			Type:           apiv1.TaskDeterministic,
			Run:            &apiv1.DeterministicRun{Command: []string{"goobers", "external-telemetry"}},
			Inputs:         map[string]string{"queryTimeout": "30"},
			InputsFrom:     map[string]string{"kind": "plan.kind"},
			TimeoutSeconds: 3600,
		}}}}
		problems := CheckStageDurationInputs(def)
		if len(problems) != 1 || !strings.Contains(problems[0], `inputs.queryTimeout "30"`) {
			t.Fatalf("problems = %q, want one queryTimeout error", problems)
		}
	})
}
