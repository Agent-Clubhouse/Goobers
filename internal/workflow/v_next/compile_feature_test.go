package vnext

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestCompileFeatureSupportLevels(t *testing.T) {
	original := currentFeatureRegistry
	t.Cleanup(func() { currentFeatureRegistry = original })

	tests := []struct {
		name           string
		level          SupportLevel
		allowPreview   bool
		wantError      bool
		wantDiagnostic string
	}{
		{name: "ga", level: SupportGA},
		{name: "preview opted in", level: SupportPreview, allowPreview: true},
		{
			name:           "preview not opted in",
			level:          SupportPreview,
			wantError:      true,
			wantDiagnostic: `DSL feature "workflow.spec.gaggle" is preview and requires explicit instance opt-in`,
		},
		{name: "deprecated", level: SupportDeprecated},
		{
			name:           "removed",
			level:          SupportRemoved,
			allowPreview:   true,
			wantError:      true,
			wantDiagnostic: `DSL feature "workflow.spec.gaggle" was removed; v1.9.0 was the last supporting version`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			features := original.All()
			for i := range features {
				features[i].Level = SupportGA
				features[i].SinceVersion = "v1.0.0"
				features[i].Replacement = ""
				features[i].RemovalTargetVersion = ""
				features[i].LastSupportingVersion = ""
				features[i].History = []SupportTransition{
					{Level: SupportPreview, SinceVersion: initialFeatureSinceVersion},
					{Level: SupportGA, SinceVersion: "v1.0.0"},
				}
				if features[i].ID != featureWorkflowGaggle {
					continue
				}
				features[i].Level = tc.level
				switch tc.level {
				case SupportPreview:
					features[i].SinceVersion = initialFeatureSinceVersion
					features[i].History = features[i].History[:1]
				case SupportDeprecated:
					features[i].SinceVersion = "v1.1.0"
					features[i].Replacement = featureWorkflowDisplayName
					features[i].RemovalTargetVersion = "v2.0.0"
					features[i].History = append(features[i].History,
						SupportTransition{Level: SupportDeprecated, SinceVersion: "v1.1.0"})
				case SupportRemoved:
					features[i].SinceVersion = "v1.2.0"
					features[i].LastSupportingVersion = "v1.9.0"
					features[i].History = append(features[i].History,
						SupportTransition{Level: SupportDeprecated, SinceVersion: "v1.1.0"},
						SupportTransition{Level: SupportRemoved, SinceVersion: "v1.2.0"})
				}
			}
			registry, err := NewFeatureRegistry(features)
			if err != nil {
				t.Fatalf("NewFeatureRegistry: %v", err)
			}
			currentFeatureRegistry = registry

			_, err = Compile(
				Definition{Name: "linear", Version: 1, Spec: featureSupportSpec()},
				WithPreviewFeatures(tc.allowPreview))

			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), tc.wantDiagnostic) {
					t.Fatalf("Compile error = %v, want diagnostic containing %q", err, tc.wantDiagnostic)
				}
				return
			}
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
		})
	}
}

func featureSupportSpec() apiv1.WorkflowSpec {
	return apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "implement"},
		},
	}
}
