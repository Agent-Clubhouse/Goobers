package instance

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestCostReportingEnabledPrecedence(t *testing.T) {
	yes, no := true, false
	settings := []struct {
		name string
		cost *apiv1.CostReporting
	}{
		{"omitted", nil},
		{"inherited", &apiv1.CostReporting{}},
		{"enabled", &apiv1.CostReporting{Enabled: &yes}},
		{"disabled", &apiv1.CostReporting{Enabled: &no}},
	}
	for _, instanceSetting := range settings {
		for _, gaggleSetting := range settings {
			t.Run(instanceSetting.name+"/"+gaggleSetting.name, func(t *testing.T) {
				config := &Config{Cost: instanceSetting.cost}
				gaggle := &apiv1.Gaggle{Spec: apiv1.GaggleSpec{Cost: gaggleSetting.cost}}
				want := true
				if instanceSetting.cost != nil && instanceSetting.cost.Enabled != nil {
					want = *instanceSetting.cost.Enabled
				}
				if gaggleSetting.cost != nil && gaggleSetting.cost.Enabled != nil {
					want = *gaggleSetting.cost.Enabled
				}
				if got := config.CostReportingEnabled(gaggle); got != want {
					t.Fatalf("effective publication = %v, want %v", got, want)
				}
			})
		}
	}
	var absent *Config
	if !absent.CostReportingEnabled(nil) {
		t.Fatal("absent configuration must preserve enabled publication")
	}
}
