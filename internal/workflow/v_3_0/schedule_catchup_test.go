package v30

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestScheduleCatchUpSemanticsAndFeatureDeclaration(t *testing.T) {
	for _, policy := range []string{"", "coalesce", "all", "unknown"} {
		def := Definition{Name: "scheduled", Version: 1, Spec: apiv1.WorkflowSpec{ScheduleCatchUp: policy, Triggers: []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: "@every 1m"}}}}
		problems := CheckSchedules(def)
		if (len(problems) == 0) != (policy != "unknown") {
			t.Fatal(policy, problems)
		}
		features, err := FeaturesForWorkflow(def)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, id := range featureIDs(features) {
			if id == "workflow.spec.scheduleCatchUp" {
				found = true
			}
		}
		if found != (policy != "") {
			t.Fatal("incorrect used feature", policy, found)
		}
	}
	def := Definition{Spec: apiv1.WorkflowSpec{ScheduleCatchUp: "all"}}
	if len(CheckSchedules(def)) == 0 {
		t.Fatal("all without schedules")
	}
	def.Spec.Triggers = make([]apiv1.Trigger, 1001)
	for i := range def.Spec.Triggers {
		def.Spec.Triggers[i] = apiv1.Trigger{Type: apiv1.TriggerSchedule, Schedule: "@every 1m"}
	}
	if len(CheckSchedules(def)) == 0 {
		t.Fatal("unbounded declaration vector")
	}
}
