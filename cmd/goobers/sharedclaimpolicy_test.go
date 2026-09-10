package main

import (
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/providers"
)

func TestClaimVisibilityUsesVerifiedWorkflowPin(t *testing.T) {
	for _, mode := range []string{"", "local", "shared"} {
		t.Run("mode-"+mode, func(t *testing.T) {
			layout := instance.NewLayout(t.TempDir())
			definition := workflow.Definition{Name: "claim", Version: 1, Spec: apiv1.WorkflowSpec{
				Gaggle: "gaggle", Start: "claim", Readiness: apiv1.ReadinessConditions{ClaimVisibility: mode},
				Tasks: []apiv1.Task{{Name: "claim", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: []string{"true"}}}},
			}}
			machine, err := workflow.Compile(definition, workflow.WithPreviewFeatures(true))
			if err != nil {
				t.Fatal(err)
			}
			newDriftTestRun(t, layout, "run", machine, true, false)
			reader, err := journal.OpenReadOnly(filepath.Join(layout.RunsDir(), "run"))
			if err != nil {
				t.Fatal(err)
			}
			identity, err := reader.Identity()
			if err != nil {
				t.Fatal(err)
			}
			want := mode
			if want == "" {
				want = "local"
			}
			if got, err := pinnedClaimVisibility(reader, identity, providers.ProviderGitHub); err != nil || got != want {
				t.Fatalf("visibility=%q err=%v", got, err)
			}
			if got, err := pinnedClaimVisibility(reader, identity, providers.ProviderKind("ado")); (err != nil) != (mode == "shared") || mode != "shared" && got != "local" {
				t.Fatalf("provider boundary: %q %v", got, err)
			}
			identity.WorkflowDigest = "sha256:wrong"
			if _, err := pinnedClaimVisibility(reader, identity, providers.ProviderGitHub); err == nil {
				t.Fatal("unverified pin selected claim policy")
			}
		})
	}
}
