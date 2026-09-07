package instance

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestEffectiveCostEnabled(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name    string
		config  Config
		gaggle  *apiv1.Gaggle
		enabled bool
	}{
		{name: "omitted defaults on", enabled: true},
		{name: "instance on", config: Config{Cost: &apiv1.CostConfig{Enabled: &yes}}, enabled: true},
		{name: "instance off", config: Config{Cost: &apiv1.CostConfig{Enabled: &no}}, enabled: false},
		{
			name: "gaggle off overrides instance on", enabled: false,
			config: Config{Cost: &apiv1.CostConfig{Enabled: &yes}},
			gaggle: &apiv1.Gaggle{Spec: apiv1.GaggleSpec{Cost: &apiv1.CostConfig{Enabled: &no}}},
		},
		{
			name: "gaggle on overrides instance off", enabled: true,
			config: Config{Cost: &apiv1.CostConfig{Enabled: &no}},
			gaggle: &apiv1.Gaggle{Spec: apiv1.GaggleSpec{Cost: &apiv1.CostConfig{Enabled: &yes}}},
		},
		{
			name: "empty gaggle block inherits", enabled: false,
			config: Config{Cost: &apiv1.CostConfig{Enabled: &no}},
			gaggle: &apiv1.Gaggle{Spec: apiv1.GaggleSpec{Cost: &apiv1.CostConfig{}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EffectiveCostEnabled(tt.config, tt.gaggle); got != tt.enabled {
				t.Fatalf("EffectiveCostEnabled() = %t, want %t", got, tt.enabled)
			}
		})
	}
}
