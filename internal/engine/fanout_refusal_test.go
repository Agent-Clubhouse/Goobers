package engine

import (
	"errors"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engineadmission"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
)

func fanOutEngineDefinition(withMap bool) wf.Definition {
	source := r9Task("plan")
	source.ArtifactSlots = []apiv1.ArtifactSlot{{Name: "plan", MediaType: "application/json"}}
	item, collate := r9Task("item"), r9Task("collate")
	item.Next = wf.TargetJoin
	spec := r9Spec(source, item, collate)
	if withMap {
		spec.Tasks[0].Next = "items"
		spec.Maps = []apiv1.Map{{Name: "items", Source: apiv1.FanOutSource{Task: "plan", Slot: "plan"}, Task: "item",
			FailurePolicy: apiv1.BranchContinueOnError, Join: "collate"}}
	} else {
		other := r9Task("other")
		other.Next = wf.TargetJoin
		spec.Tasks = append(spec.Tasks, other)
		spec.Tasks[0].Next = "review"
		spec.Parallels = []apiv1.Parallel{{Name: "review", FailurePolicy: apiv1.BranchContinueOnError, Join: "collate", MaxConcurrentBranches: 1,
			Selection: &apiv1.ParallelSelection{Source: apiv1.FanOutSource{Task: "plan", Slot: "plan"}},
			Branches:  []apiv1.Branch{{Name: "item", Start: "item"}, {Name: "other", Start: "other", Optional: true}}}}
	}
	return wf.Definition{Name: "fanout", DSLVersion: "3.1", Spec: spec}
}

// TestFanOutRefusedAtEngineBoundaries pins design §13 for maps at run start,
// in the daemon preflight, and inside a direct Temporal invocation.
func TestFanOutRefusedAtEngineBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		def      wf.Definition
		sentinel error
		feature  string
		state    string
		direct   error
	}{
		{"map", fanOutEngineDefinition(true), ErrMapsUnsupported, "spec.maps", "items", wf.ErrMapUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := engineadmission.RefuseDefinition(tc.def.Name, tc.def.Spec)
			found := false
			var refusals *UnsupportedFeaturesError
			if errors.As(err, &refusals) {
				for _, refusal := range refusals.Refusals {
					found = found || (refusal.Feature == tc.feature && refusal.Stage == tc.state)
				}
			}
			if !found {
				t.Fatalf("StartInput refusal %v does not name %s on %q", err, tc.feature, tc.state)
			}
			if err := engineadmission.RefuseDefinition(tc.def.Name, tc.def.Spec); !errors.Is(err, tc.sentinel) {
				t.Fatalf("daemon preflight: %v", err)
			}
			var ts testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&ts)
			env.ExecuteWorkflow(Run, RunInput{RunID: "fanout-guard", WorkflowName: tc.def.Name, DSLVersion: tc.def.DSLVersion, Spec: tc.def.Spec})
			if err := env.GetWorkflowError(); err == nil || !strings.Contains(err.Error(), tc.direct.Error()) {
				t.Fatalf("direct Temporal run: %v", err)
			}
		})
	}
}

func TestSelectiveParallelHasNoFeatureSpecificEngineRefusal(t *testing.T) {
	def := fanOutEngineDefinition(false)
	err := engineadmission.RefuseDefinition(def.Name, def.Spec)
	var refusals *UnsupportedFeaturesError
	if !errors.As(err, &refusals) {
		t.Fatalf("RefuseDefinition = %v, want base parallel refusal", err)
	}
	for _, refusal := range refusals.Refusals {
		if refusal.Feature == "spec.parallels.selection" {
			t.Fatalf("selection remains refused after engine support: %v", refusal)
		}
	}
	if !errors.Is(err, ErrParallelsUnsupported) {
		t.Fatalf("base parallel refusal changed before #7753: %v", err)
	}
}
