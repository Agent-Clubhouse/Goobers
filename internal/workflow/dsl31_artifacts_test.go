package workflow

import (
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/supportmatrix"
)

func TestDSL31ArtifactContractsCompileAndResolveFeatures(t *testing.T) {
	def := Definition{
		Name:       "artifact-contract",
		DSLVersion: supportmatrix.V31DSLVersion,
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "web",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:    "_produce",
			Tasks: []apiv1.Task{
				{
					Name: "_produce", Type: apiv1.TaskDeterministic, Goal: "produce",
					Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
					ArtifactSlots: []apiv1.ArtifactSlot{
						{Name: "_report", MediaType: "application/json", SchemaPath: "schemas/report.schema.json", MaxSize: 1024},
						{Name: "trace", MediaType: "application/x-ndjson"},
					},
					Next: "consume",
				},
				{
					Name: "consume", Type: apiv1.TaskDeterministic, Goal: "consume",
					Run:            &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
					ArtifactInputs: map[string]apiv1.ArtifactInputRef{"_summary": {From: "_produce._report"}, "telemetry": {From: "_produce.trace"}},
				},
			},
		},
	}
	if _, err := Compile(def, WithPreviewFeatures(true)); err != nil {
		t.Fatalf("Compile: %v", err)
	}
	features, err := FeaturesForWorkflow(def)
	if err != nil {
		t.Fatalf("FeaturesForWorkflow: %v", err)
	}
	got := featureIDStrings(features)
	for _, want := range []string{
		"task.artifactSlots",
		"task.artifactSlots.mediaType",
		"task.artifactSlots.schemaPath",
		"task.artifactSlots.maxSize",
		"task.artifactInputs",
		"task.artifactInputs.from",
	} {
		if !slices.Contains(got, want) {
			t.Fatalf("features = %v, want %s", got, want)
		}
	}
}

func TestDSL30RejectsArtifactContractSurface(t *testing.T) {
	baseTask := apiv1.Task{
		Name: "produce", Type: apiv1.TaskDeterministic, Goal: "produce",
		Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
	}
	for _, tc := range []struct {
		name      string
		mutate    func(*apiv1.Task)
		wantError string
	}{
		{
			name:      "non-empty artifactSlots",
			mutate:    func(task *apiv1.Task) { task.ArtifactSlots = []apiv1.ArtifactSlot{{Name: "report"}} },
			wantError: `artifactSlots, which requires dslVersion "3.1"`,
		},
		{
			name:      "empty artifactSlots",
			mutate:    func(task *apiv1.Task) { task.ArtifactSlots = []apiv1.ArtifactSlot{} },
			wantError: `artifactSlots, which requires dslVersion "3.1"`,
		},
		{
			name:      "empty artifactInputs",
			mutate:    func(task *apiv1.Task) { task.ArtifactInputs = map[string]apiv1.ArtifactInputRef{} },
			wantError: `artifactInputs, which requires dslVersion "3.1"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := baseTask
			tc.mutate(&task)
			def := Definition{
				Name:       "artifact-contract",
				DSLVersion: supportmatrix.V3DSLVersion,
				Spec: apiv1.WorkflowSpec{
					Gaggle:   "web",
					Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
					Start:    "produce",
					Tasks:    []apiv1.Task{task},
				},
			}
			_, err := Compile(def, WithPreviewFeatures(true))
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("Compile error = %v, want DSL 3.1 surface refusal containing %q", err, tc.wantError)
			}
		})
	}
}

func TestDSL31RejectsUnknownArtifactInputSlot(t *testing.T) {
	def := Definition{
		Name:       "artifact-contract",
		DSLVersion: supportmatrix.V31DSLVersion,
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "web",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:    "produce",
			Tasks: []apiv1.Task{
				{
					Name: "produce", Type: apiv1.TaskDeterministic, Goal: "produce",
					Run:           &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
					ArtifactSlots: []apiv1.ArtifactSlot{{Name: "report"}},
					Next:          "consume",
				},
				{
					Name: "consume", Type: apiv1.TaskDeterministic, Goal: "consume",
					Run:            &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
					ArtifactInputs: map[string]apiv1.ArtifactInputRef{"summary": {From: "produce.missing"}},
				},
			},
		},
	}
	_, err := Compile(def, WithPreviewFeatures(true))
	if err == nil || !strings.Contains(err.Error(), `artifact input "summary" references unknown artifact slot "missing" on producer "produce"`) {
		t.Fatalf("Compile error = %v, want unknown slot refusal", err)
	}
}

func featureIDStrings(features []Feature) []string {
	out := make([]string, len(features))
	for i, feature := range features {
		out[i] = string(feature.ID)
	}
	return out
}
