package workflow

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestCostPublicationFeatureIsIndependentOfWorkflowPin(t *testing.T) {
	feature, ok := LookupFeature("gaggle.spec.cost.enabled")
	if !ok || feature.Level != SupportGA {
		t.Fatalf("feature = %+v, present=%v", feature, ok)
	}
	for _, version := range []string{"2.0", "3.0"} {
		def := Definition{DSLVersion: version}
		features, err := FeaturesForGaggle(def, apiv1.GaggleSpec{Cost: &apiv1.CostReporting{}})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, used := range features {
			if used.ID == feature.ID {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s omitted cost feature", version)
		}
		if findings := CheckFeatureSupport(def, []Feature{feature}, false); len(findings) != 0 {
			t.Fatalf("%s rejects cost policy: %+v", version, findings)
		}
	}
}
