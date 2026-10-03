package workflow

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestHealthPolicyFeatureIsIndependentOfWorkflowPin(t *testing.T) {
	feature, ok := LookupFeature("gaggle.spec.health")
	if !ok {
		t.Fatal("health policy is absent from the feature registry")
	}
	for _, version := range []string{"2.0", "3.0"} {
		features, err := FeaturesForGaggle(Definition{DSLVersion: version}, apiv1.GaggleSpec{
			Health: &apiv1.GaggleHealthPolicy{},
		})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, used := range features {
			found = found || used.ID == feature.ID
		}
		if !found {
			t.Fatalf("%s omitted health policy feature", version)
		}
	}
}
