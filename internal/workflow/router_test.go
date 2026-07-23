package workflow

import (
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	vcurrent "github.com/goobers/goobers/internal/workflow/v_current"
)

func TestCompileDispatchesPinnedInterpreterVersion(t *testing.T) {
	def := Definition{
		Name:       "current",
		Version:    1,
		DSLVersion: vcurrent.DSLVersion,
		Spec:       linearSpec(),
	}

	got, err := Compile(def, WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	want, err := vcurrent.Compile(def, vcurrent.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("v_current.Compile: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("router machine differs from v_current interpreter:\n got  %#v\n want %#v", got, want)
	}
}

func TestCompileAdaptsRouterOptionsToCurrentInterpreter(t *testing.T) {
	t.Run("goobers and known harnesses", func(t *testing.T) {
		def := Definition{
			Name:       "harness",
			Version:    1,
			DSLVersion: vcurrent.DSLVersion,
			Spec:       linearSpec(),
		}
		goobers := map[string]apiv1.GooberSpec{
			"coder": {Role: "coder", Harness: apiv1.Harness("alternate")},
		}

		if _, err := Compile(def, WithGoobers(goobers), WithKnownHarnesses([]string{"alternate"})); err != nil {
			t.Fatalf("Compile with registered harness: %v", err)
		}
		if _, err := Compile(def, WithGoobers(goobers), WithKnownHarnesses(nil)); err == nil ||
			!strings.Contains(err.Error(), `unknown harness "alternate"`) {
			t.Fatalf("Compile with empty harness registry error = %v, want unknown-harness diagnostic", err)
		}
	})

	t.Run("known checks", func(t *testing.T) {
		def := Definition{
			Name:       "checks",
			Version:    1,
			DSLVersion: vcurrent.DSLVersion,
			Spec: apiv1.WorkflowSpec{
				Start: "gate",
				Tasks: []apiv1.Task{{
					Name: "sink",
					Type: apiv1.TaskDeterministic,
					Goal: "finish",
					Run:  &apiv1.DeterministicRun{Command: []string{"true"}},
				}},
				Gates: []apiv1.Gate{{
					Name:      "gate",
					Evaluator: apiv1.EvaluatorAutomated,
					Automated: &apiv1.AutomatedGate{Check: "custom"},
					Branches:  map[string]string{"pass": "sink", "fail": "sink"},
				}},
			},
		}

		if _, err := Compile(def, WithKnownChecks([]string{"custom"})); err != nil {
			t.Fatalf("Compile with registered check: %v", err)
		}
		if _, err := Compile(def, WithKnownChecks(nil)); err == nil ||
			!strings.Contains(err.Error(), `unknown automated check "custom"`) {
			t.Fatalf("Compile with empty check registry error = %v, want unknown-check diagnostic", err)
		}
	})

	t.Run("preview features", func(t *testing.T) {
		def := Definition{
			Name:       "preview",
			Version:    1,
			DSLVersion: vcurrent.DSLVersion,
			Spec: apiv1.WorkflowSpec{
				Start: "build",
				Tasks: []apiv1.Task{{
					Name: "build",
					Type: apiv1.TaskDeterministic,
					Goal: "build",
					Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Image: "alpine:3.20"},
				}},
			},
		}

		if _, err := Compile(def, WithPreviewFeatures(true)); err != nil {
			t.Fatalf("Compile with preview opt-in: %v", err)
		}
	})
}

func TestCompileUnpinnedUsesCurrentInterpreterWithoutChangingDefinition(t *testing.T) {
	def := Definition{Name: "unpinned", Version: 1, Spec: linearSpec()}
	machine, err := Compile(def, WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if machine.Def.DSLVersion != "" {
		t.Fatalf("compiled DSL version = %q, want transitional unpinned definition preserved", machine.Def.DSLVersion)
	}
}

func TestCompileRejectsUnknownDSLVersion(t *testing.T) {
	def := Definition{Name: "unknown", Version: 1, DSLVersion: "9.9", Spec: linearSpec()}
	_, err := Compile(def, WithPreviewFeatures(true))
	if err == nil || !strings.Contains(err.Error(), `DSL version "9.9" is not supported by this build`) {
		t.Fatalf("Compile error = %v, want unsupported-version diagnostic", err)
	}
}

func TestDefinitionFacadesRejectUnknownDSLVersion(t *testing.T) {
	def := Definition{Name: "unknown", Version: 1, DSLVersion: "9.9", Spec: linearSpec()}
	checks := map[string]func(Definition) []string{
		"warnings":                CheckWarnings,
		"reachability":            CheckReachability,
		"schedules":               CheckSchedules,
		"trigger fields":          CheckTriggerFields,
		"admission":               func(def Definition) []string { return CheckWorkflowAdmission(def, nil) },
		"gate parameters":         CheckGateParameters,
		"gate outcomes":           CheckGateOutcomes,
		"required inputs":         CheckStageRequiredInputs,
		"stage contracts":         CheckStageContracts,
		"stage contract warnings": CheckStageContractWarnings,
		"timeout coherence":       CheckStageTimeoutCoherence,
	}
	for name, check := range checks {
		t.Run(name, func(t *testing.T) {
			problems := check(def)
			if len(problems) != 1 || !strings.Contains(problems[0], `DSL version "9.9" is not supported`) {
				t.Fatalf("problems = %q, want unsupported-version diagnostic", problems)
			}
		})
	}

	if _, err := FeaturesForWorkflow(def); err == nil || !strings.Contains(err.Error(), `DSL version "9.9" is not supported`) {
		t.Fatalf("FeaturesForWorkflow error = %v, want unsupported-version diagnostic", err)
	}
	diagnostics := CheckWorkflowFeatureSupport(def, true)
	if len(diagnostics) != 1 || !diagnostics[0].Blocking || !strings.Contains(diagnostics[0].Message, `DSL version "9.9" is not supported`) {
		t.Fatalf("feature diagnostics = %+v, want blocking unsupported-version diagnostic", diagnostics)
	}
}

func TestRuntimeFacadesDispatchByMachineVersion(t *testing.T) {
	def := Definition{
		Name:       "runtime",
		Version:    1,
		DSLVersion: vcurrent.DSLVersion,
		Spec: apiv1.WorkflowSpec{
			Start: "poll",
			Tasks: []apiv1.Task{{
				Name: "poll",
				Type: apiv1.TaskDeterministic,
				Goal: "poll",
				Inputs: map[string]string{
					"kind": "ci-poll",
				},
				Capabilities:   []string{"github:pr:write"},
				TimeoutSeconds: 20,
				Next:           "ci",
			}},
			Gates: []apiv1.Gate{{
				Name:      "ci",
				Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{
					Check:               "ci-status",
					TimeoutSeconds:      10,
					PollIntervalSeconds: 3,
				},
				Branches: map[string]string{"pass": "", "fail": "@abort", "timeout": "@escalate"},
			}},
		},
	}
	machine, err := Compile(def, WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	task := def.Spec.Tasks[0]
	gate := def.Spec.Gates[0]

	inputs, err := TaskInvocationInputs(machine, task)
	if err != nil {
		t.Fatalf("TaskInvocationInputs: %v", err)
	}
	if want := vcurrent.TaskInvocationInputs(machine, task); !reflect.DeepEqual(inputs, want) {
		t.Fatalf("TaskInvocationInputs = %v, want %v", inputs, want)
	}
	limits, err := TaskLimits(machine, task)
	if err != nil {
		t.Fatalf("TaskLimits: %v", err)
	}
	if want := vcurrent.TaskLimits(task); limits != want {
		t.Fatalf("TaskLimits = %+v, want %+v", limits, want)
	}
	gateLimits, err := GateLimits(machine, gate)
	if err != nil {
		t.Fatalf("GateLimits: %v", err)
	}
	if want := vcurrent.GateLimits(gate); gateLimits != want {
		t.Fatalf("GateLimits = %+v, want %+v", gateLimits, want)
	}
}

func TestRuntimeFacadesRejectUnknownMachineVersion(t *testing.T) {
	machine := &Machine{Def: Definition{DSLVersion: "9.9"}}
	task := apiv1.Task{Name: "task"}
	gate := apiv1.Gate{Name: "gate"}

	if _, err := TaskInvocationInputs(machine, task); err == nil {
		t.Fatal("TaskInvocationInputs succeeded for an unknown interpreter")
	}
	if _, err := TaskLimits(machine, task); err == nil {
		t.Fatal("TaskLimits succeeded for an unknown interpreter")
	}
	if _, err := GateLimits(machine, gate); err == nil {
		t.Fatal("GateLimits succeeded for an unknown interpreter")
	}
}
