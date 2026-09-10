package v30

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestClaimVisibilityAdmissionAndFeatureInventory(t *testing.T) {
	for _, mode := range []string{"", "local", "shared", "global", "SHARED"} {
		t.Run("mode="+mode, func(t *testing.T) {
			spec := singleTaskSpec(apiv1.Task{Name: "implement", Type: apiv1.TaskDeterministic,
				Run: &apiv1.DeterministicRun{Command: []string{"true"}}})
			spec.Readiness.ClaimVisibility = mode
			definition := Definition{Name: "claim-visibility", Version: 1, Spec: spec}
			_, err := compileAcknowledged(definition)
			valid := mode == "" || mode == "local" || mode == "shared"
			if !valid {
				if err == nil || !strings.Contains(err.Error(), "claimVisibility must be local or shared") {
					t.Fatalf("unknown visibility admitted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			features, err := FeaturesForWorkflow(definition)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, feature := range features {
				if feature.ID == featureWorkflowClaimVisibility {
					found = true
				}
			}
			if found != (mode != "") {
				t.Fatalf("claim visibility feature recorded=%t for %q", found, mode)
			}
		})
	}
}
