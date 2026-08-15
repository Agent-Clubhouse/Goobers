package workflow

import (
	"os"
	"path/filepath"
	"slices"
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
			wantDigest:   "sha256:dfdf7dec7f2d1035d464b140bfe16f5c5a6f0f4058426d2dce9bae08af27846c",
			wantInterval: "7s",
		},
		{
			name:         "next",
			path:         filepath.Join("v_next", "testdata", "golden", "runtime-policy.yaml"),
			wantDigest:   "sha256:de0f8f6f656ab70841dd5a74886a4ec8118bd961fe9a0817f73465e21901b63f",
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
			Capabilities: []string{"provider:pr:write"},
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

func TestNextFeatureRegistryDoesNotRevalidateWithCurrentInterpreter(t *testing.T) {
	input := []Feature{{ID: "rejected-by-current"}}
	if _, err := vcurrent.NewFeatureRegistry(input); err == nil {
		t.Fatal("test feature unexpectedly passed current validation")
	}

	const nextOnly FeatureID = "goober.spec.next-only"
	registry, err := newNextFeatureRegistryWith(
		input,
		func([]vnext.Feature) (vnext.FeatureRegistry, error) {
			return vnext.NewFeatureRegistry([]vnext.Feature{{
				ID:           vnext.FeatureID(nextOnly),
				Level:        vnext.SupportPreview,
				SinceVersion: "v0.1.0",
				History: []vnext.SupportTransition{{
					Level:        vnext.SupportPreview,
					SinceVersion: "v0.1.0",
				}},
				DSLVersions: []vnext.DSLFeatureSupport{{
					Version: vnext.DSLVersion,
					Level:   vnext.SupportPreview,
				}},
			}})
		},
	)
	if err != nil {
		t.Fatalf("next registry rejected interpreter-validated features: %v", err)
	}
	if feature, ok := registry.Lookup(nextOnly); !ok || feature.ID != nextOnly {
		t.Fatalf("Lookup(%q) = %+v, %v", nextOnly, feature, ok)
	}
}

func TestGooberFeaturesRouteThroughPinnedInterpreter(t *testing.T) {
	const nextOnly FeatureID = "goober.spec.next-only"
	original := nextInterpreter.featuresForGoober
	nextInterpreter.featuresForGoober = func(apiv1.GooberSpec) ([]Feature, error) {
		return []Feature{{
			ID:           nextOnly,
			Level:        SupportPreview,
			SinceVersion: "v0.1.0",
			DSLVersions: []DSLFeatureSupport{{
				Version: vnext.DSLVersion,
				Level:   SupportPreview,
			}},
		}}, nil
	}
	t.Cleanup(func() { nextInterpreter.featuresForGoober = original })

	spec := apiv1.GooberSpec{}
	current, err := FeaturesForGoober(Definition{DSLVersion: vcurrent.DSLVersion}, spec)
	if err != nil {
		t.Fatal(err)
	}
	next, err := FeaturesForGoober(Definition{DSLVersion: vnext.DSLVersion}, spec)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(current, func(feature Feature) bool { return feature.ID == nextOnly }) {
		t.Fatalf("DSL %s unexpectedly resolved %q", vcurrent.DSLVersion, nextOnly)
	}
	if !slices.ContainsFunc(next, func(feature Feature) bool { return feature.ID == nextOnly }) {
		t.Fatalf("DSL %s did not resolve %q", vnext.DSLVersion, nextOnly)
	}
	if diagnostics := CheckGooberFeatureSupport(
		Definition{DSLVersion: vcurrent.DSLVersion}, spec, false,
	); len(diagnostics) != 0 {
		t.Fatalf("DSL %s diagnostics = %+v, want none", vcurrent.DSLVersion, diagnostics)
	}
	diagnostics := CheckGooberFeatureSupport(
		Definition{DSLVersion: vnext.DSLVersion}, spec, false,
	)
	if len(diagnostics) != 1 || diagnostics[0].Feature.ID != nextOnly || !diagnostics[0].Blocking {
		t.Fatalf("DSL %s diagnostics = %+v, want blocking %q", vnext.DSLVersion, diagnostics, nextOnly)
	}
}
