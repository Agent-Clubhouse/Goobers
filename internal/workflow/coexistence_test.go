package workflow

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	v30 "github.com/goobers/goobers/internal/workflow/v_3_0"
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
			name:         "next",
			path:         filepath.Join("v_next", "testdata", "golden", "runtime-policy.yaml"),
			wantDigest:   "sha256:de0f8f6f656ab70841dd5a74886a4ec8118bd961fe9a0817f73465e21901b63f",
			wantInterval: "10s",
		},
		{
			name:         "v30",
			path:         filepath.Join("v_3_0", "testdata", "golden", "runtime-policy.yaml"),
			wantDigest:   "sha256:cec77a4a6f15e2c08b7e11951de9c632df69c8ec0d9941ed28ca22440f0fd21c",
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

// TestUnpinnedResolvesToBackCompatInterpreter: with DSL 1.4 dropped (#3507) an
// unpinned definition routes to the 2.0 (v_next) interpreter — the back-compat
// contract — so it and an explicitly 2.0-pinned definition compute the same
// ci-poll interval default.
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
		{name: "unpinned", wantInterval: "10s"},
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
	for _, version := range []string{vnext.DSLVersion, v30.DSLVersion} {
		if !got[version] {
			t.Errorf("stage.ci-poll does not report DSL version %s", version)
		}
	}
}

// TestPreV30SurfaceRefusedOnEarlierVersions pins the router-owned version
// gate: the 3.0-only fields must be refused on the 2.0 document — the frozen
// interpreter never learns them (PO-D0) — and the gaggle runsOn floor must
// refuse to pair with a pre-3.0 workflow (dsl-3.0.md open point 2). (1.4 is
// dropped, #3507; a 1.4 document is refused earlier, at version resolution.)
func TestPreV30SurfaceRefusedOnEarlierVersions(t *testing.T) {
	spec := func(mutate func(*apiv1.WorkflowSpec)) apiv1.WorkflowSpec {
		s := apiv1.WorkflowSpec{
			Gaggle:   "web",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
			Start:    "implement",
			Tasks: []apiv1.Task{{
				Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "implement",
			}},
		}
		if mutate != nil {
			mutate(&s)
		}
		return s
	}

	for _, version := range []string{vnext.DSLVersion} {
		withRunsOn := spec(func(s *apiv1.WorkflowSpec) {
			s.Tasks[0].RunsOn = &apiv1.RunsOn{OS: "linux"}
		})
		_, err := Compile(Definition{Name: "runs-on", Version: 1, DSLVersion: version, Spec: withRunsOn},
			WithPreviewFeatures(true))
		if err == nil || !strings.Contains(err.Error(), `declares runsOn, which requires dslVersion "3.0"`) {
			t.Fatalf("Compile(%s, runsOn) error = %v, want version-gate refusal", version, err)
		}

		withRepoFrom := spec(func(s *apiv1.WorkflowSpec) {
			s.Tasks[0].RepoFrom = apiv1.RepoFrom{"other"}
		})
		_, err = Compile(Definition{Name: "repo-from", Version: 1, DSLVersion: version, Spec: withRepoFrom},
			WithPreviewFeatures(true))
		if err == nil || !strings.Contains(err.Error(), `declares repoFrom, which requires dslVersion "3.0"`) {
			t.Fatalf("Compile(%s, repoFrom) error = %v, want version-gate refusal", version, err)
		}

		_, err = Compile(Definition{Name: "floor", Version: 1, DSLVersion: version, Spec: spec(nil)},
			WithPreviewFeatures(true),
			WithGaggleRunsOn(&apiv1.GaggleRunsOn{OS: "linux"}))
		if err == nil || !strings.Contains(err.Error(), "the gaggle declares runsOn, which requires every workflow in the gaggle to pin dslVersion \"3.0\"") {
			t.Fatalf("Compile(%s, gaggle floor) error = %v, want pairing refusal", version, err)
		}

		// A nil floor (the value every pre-Goobernetes gaggle produces) stays
		// accepted — byte-identity for untouched configs.
		if _, err := Compile(Definition{Name: "clean", Version: 1, DSLVersion: version, Spec: spec(nil)},
			WithPreviewFeatures(true), WithGaggleRunsOn(nil)); err != nil {
			t.Fatalf("Compile(%s, clean) = %v, want success", version, err)
		}
	}

	// The same document on 3.0 compiles, floor included — end to end through
	// the facade the daemon uses.
	withRunsOn := spec(func(s *apiv1.WorkflowSpec) {
		s.Tasks[0].RunsOn = &apiv1.RunsOn{OS: "linux", CPU: "2000m", Capabilities: []string{"go@1.26"}}
	})
	machine, err := Compile(Definition{Name: "v30", Version: 1, DSLVersion: v30.DSLVersion, Spec: withRunsOn},
		WithPreviewFeatures(true),
		WithGaggleRunsOn(&apiv1.GaggleRunsOn{Capabilities: []string{"make"}}))
	if err != nil {
		t.Fatalf("Compile(3.0) = %v, want success", err)
	}
	if machine.Digest() == "" {
		t.Fatal("compiled 3.0 machine has no digest")
	}
}

func TestNextFeatureRegistryDoesNotRevalidateWithCurrentInterpreter(t *testing.T) {
	// A feature whose lifecycle metadata is intentionally incomplete: it would
	// be rejected by a full registry validation, proving that
	// newNextFeatureRegistryWith runs the SUPPLIED validate func rather than a
	// global one.
	input := []Feature{{ID: "rejected-by-current"}}
	if _, err := vnext.NewFeatureRegistry([]vnext.Feature{{ID: vnext.FeatureID("rejected-by-current")}}); err == nil {
		t.Fatal("test feature unexpectedly passed registry validation")
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
	current, err := FeaturesForGoober(Definition{DSLVersion: v30.DSLVersion}, spec)
	if err != nil {
		t.Fatal(err)
	}
	next, err := FeaturesForGoober(Definition{DSLVersion: vnext.DSLVersion}, spec)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(current, func(feature Feature) bool { return feature.ID == nextOnly }) {
		t.Fatalf("DSL %s unexpectedly resolved %q", v30.DSLVersion, nextOnly)
	}
	if !slices.ContainsFunc(next, func(feature Feature) bool { return feature.ID == nextOnly }) {
		t.Fatalf("DSL %s did not resolve %q", vnext.DSLVersion, nextOnly)
	}
	if diagnostics := CheckGooberFeatureSupport(
		Definition{DSLVersion: v30.DSLVersion}, spec, false,
	); len(diagnostics) != 0 {
		t.Fatalf("DSL %s diagnostics = %+v, want none", v30.DSLVersion, diagnostics)
	}
	diagnostics := CheckGooberFeatureSupport(
		Definition{DSLVersion: vnext.DSLVersion}, spec, false,
	)
	if len(diagnostics) != 1 || diagnostics[0].Feature.ID != nextOnly || !diagnostics[0].Blocking {
		t.Fatalf("DSL %s diagnostics = %+v, want blocking %q", vnext.DSLVersion, diagnostics, nextOnly)
	}
}
