package v1alpha1

import "testing"

func TestResolveBackprop(t *testing.T) {
	active := &GaggleBackprop{Mode: BackpropModeActive, Granularity: BackpropGranularityStage}
	tests := []struct {
		name     string
		workflow *BackpropConfig
		gaggle   *GaggleBackprop
		want     BackpropConfig
	}{
		{name: "neither is off", want: BackpropConfig{Mode: BackpropModeOff}},
		{name: "gaggle off applies nothing", gaggle: &GaggleBackprop{Mode: BackpropModeOff}, want: BackpropConfig{Mode: BackpropModeOff}},
		{
			name: "omitted workflow inherits gaggle defaults", gaggle: active,
			want: BackpropConfig{Enabled: true, Mode: BackpropModeActive, Version: DefaultBackpropVersion, Granularity: BackpropGranularityStage},
		},
		{
			name:     "enabled false with own granularity inherits mode but keeps granularity and version",
			workflow: &BackpropConfig{Enabled: false, Version: "v1", Granularity: BackpropGranularityRun},
			gaggle:   &GaggleBackprop{Mode: BackpropModeActive, Version: "v2", Granularity: BackpropGranularityStage},
			want:     BackpropConfig{Enabled: true, Mode: BackpropModeActive, Version: "v1", Granularity: BackpropGranularityRun},
		},
		{
			name:     "explicit off opts out",
			workflow: &BackpropConfig{Mode: BackpropModeOff}, gaggle: active,
			want: BackpropConfig{Mode: BackpropModeOff, Version: DefaultBackpropVersion, Granularity: BackpropGranularityStage},
		},
		{
			name:     "workflow shadow overrides gaggle active",
			workflow: &BackpropConfig{Mode: BackpropModeShadow, Version: "v1"}, gaggle: active,
			want: BackpropConfig{Mode: BackpropModeShadow, Version: "v1", Granularity: BackpropGranularityStage},
		},
		{
			name:     "legacy enabled keeps active and inherits omitted granularity",
			workflow: &BackpropConfig{Enabled: true, Version: "v1"}, gaggle: &GaggleBackprop{Mode: BackpropModeShadow, Granularity: BackpropGranularityRun},
			want: BackpropConfig{Enabled: true, Mode: BackpropModeActive, Version: "v1", Granularity: BackpropGranularityRun},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ResolveBackprop(test.workflow, test.gaggle); got != test.want {
				t.Fatalf("ResolveBackprop() = %+v, want %+v", got, test.want)
			}
		})
	}
}
