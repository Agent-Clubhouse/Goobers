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

func TestEnabledFeaturesAreIndependentOfWorkflowPin(t *testing.T) {
	enabled := false
	for _, id := range []FeatureID{"gaggle.spec.enabled", "workflow.spec.enabled"} {
		feature, ok := LookupFeature(id)
		if !ok || feature.Level != SupportGA {
			t.Fatalf("%s feature = %+v, present=%v", id, feature, ok)
		}
		for _, version := range []string{"2.0", "3.0"} {
			def := Definition{DSLVersion: version}
			var features []Feature
			var err error
			if id == "gaggle.spec.enabled" {
				features, err = FeaturesForGaggle(def, apiv1.GaggleSpec{Enabled: &enabled})
			} else {
				def.Spec.Enabled = &enabled
				features, err = FeaturesForWorkflow(def)
			}
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
				t.Fatalf("%s omitted %s feature", version, id)
			}
			if findings := CheckFeatureSupport(def, []Feature{feature}, false); len(findings) != 0 {
				t.Fatalf("%s rejects %s policy: %+v", version, id, findings)
			}
		}
	}
}
