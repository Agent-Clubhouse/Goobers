package workflow

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	vcurrent "github.com/goobers/goobers/internal/workflow/v_current"
	vnext "github.com/goobers/goobers/internal/workflow/v_next"
)

func TestVersionedInterpreterFixturesCompileInOneBinary(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		wantDigest   string
		wantInterval string
	}{
		{
			name:         "current",
			path:         filepath.Join("v_current", "testdata", "golden", "runtime-policy.yaml"),
			wantDigest:   "sha256:a28ff7bcb38e9441542f53fc968ba78354c2c61515030bb40f6d06557de051c8",
			wantInterval: "7s",
		},
		{
			name:         "next",
			path:         filepath.Join("v_next", "testdata", "golden", "runtime-policy.yaml"),
			wantDigest:   "sha256:bfc92dc4f85277bc15aa4fe025bebdffec4bd8467c104ade629c84bfd60a99ae",
			wantInterval: "10s",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw, err := os.ReadFile(test.path)
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			var parsed apiv1.Workflow
			if err := yaml.Unmarshal(raw, &parsed); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			machine, err := Compile(Definition{
				Name:       parsed.Name,
				Version:    1,
				DSLVersion: parsed.DSLVersion,
				Spec:       parsed.Spec,
			}, WithPreviewFeatures(true))
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if got := machine.Digest(); got != test.wantDigest {
				t.Fatalf("machine digest = %q, want %q", got, test.wantDigest)
			}
			task, ok := machine.Task("poll")
			if !ok {
				t.Fatal("compiled machine has no poll task")
			}
			inputs, err := TaskInvocationInputs(machine, task)
			if err != nil {
				t.Fatalf("TaskInvocationInputs: %v", err)
			}
			if got := inputs["pollIntervalSeconds"]; got != test.wantInterval {
				t.Fatalf("poll interval = %q, want %q", got, test.wantInterval)
			}
		})
	}
}

func TestNextDefaultDoesNotAlterCurrentInterpreter(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Start: "poll",
		Tasks: []apiv1.Task{{
			Name: "poll",
			Type: apiv1.TaskDeterministic,
			Goal: "poll",
			Run:  &apiv1.DeterministicRun{Command: []string{"true"}},
			Inputs: map[string]string{
				"kind":                "ci-poll",
				"pollIntervalSeconds": "30s",
			},
			Capabilities: []string{"github:pr:write"},
			Next:         "ci",
		}},
		Gates: []apiv1.Gate{{
			Name:      "ci",
			Evaluator: apiv1.EvaluatorAutomated,
			Automated: &apiv1.AutomatedGate{Check: "ci-status"},
			Branches:  map[string]string{"pass": "", "fail": "@abort", "timeout": "@escalate"},
		}},
	}

	tests := []struct {
		name         string
		version      string
		wantInterval string
	}{
		{name: "unpinned", wantInterval: "30s"},
		{name: "current", version: vcurrent.DSLVersion, wantInterval: "30s"},
		{name: "next", version: vnext.DSLVersion, wantInterval: "10s"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			machine, err := Compile(Definition{
				Name:       "poll-default",
				Version:    1,
				DSLVersion: test.version,
				Spec:       spec,
			}, WithPreviewFeatures(true))
			if err != nil {
				t.Fatalf("Compile(%s): %v", test.version, err)
			}
			task, _ := machine.Task("poll")
			inputs, err := TaskInvocationInputs(machine, task)
			if err != nil {
				t.Fatalf("TaskInvocationInputs(%s): %v", test.version, err)
			}
			if got := inputs["pollIntervalSeconds"]; got != test.wantInterval {
				t.Fatalf("DSL %s poll interval = %q, want %q", test.version, got, test.wantInterval)
			}
		})
	}
}

func TestFeatureRegistryReportsBothInterpreterVersions(t *testing.T) {
	feature, ok := LookupFeature("stage.ci-poll")
	if !ok {
		t.Fatal("stage.ci-poll feature is missing")
	}
	got := map[string]bool{}
	for _, support := range feature.DSLVersions {
		got[support.Version] = true
	}
	for _, version := range []string{vcurrent.DSLVersion, vnext.DSLVersion} {
		if !got[version] {
			t.Errorf("stage.ci-poll does not report DSL version %s", version)
		}
	}
}
