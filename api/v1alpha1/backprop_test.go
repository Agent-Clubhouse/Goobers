package v1alpha1

import (
	"reflect"
	"testing"
)

func TestResolveBackprop(t *testing.T) {
	active := &GaggleBackprop{Mode: BackpropModeActive, Granularity: BackpropGranularityStage}
	humanOnly := &BackpropGroundTruth{Sources: []BackpropLabelSource{BackpropLabelSourceHuman}}
	optOut := &BackpropGroundTruth{Sources: []BackpropLabelSource{}}
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
		{
			name:     "omitted workflow inherits gaggle ground truth",
			workflow: nil, gaggle: &GaggleBackprop{Mode: BackpropModeActive, GroundTruth: humanOnly},
			want: BackpropConfig{Enabled: true, Mode: BackpropModeActive, Version: DefaultBackpropVersion, GroundTruth: humanOnly},
		},
		{
			name:     "explicit mode inherits omitted ground truth",
			workflow: &BackpropConfig{Mode: BackpropModeActive, Version: "v1"}, gaggle: &GaggleBackprop{Mode: BackpropModeOff, GroundTruth: humanOnly},
			want: BackpropConfig{Mode: BackpropModeActive, Version: "v1", GroundTruth: humanOnly},
		},
		{
			name:     "workflow ground-truth opt-out overrides gaggle sources",
			workflow: &BackpropConfig{Version: "v1", GroundTruth: optOut}, gaggle: &GaggleBackprop{Mode: BackpropModeActive, GroundTruth: humanOnly},
			want: BackpropConfig{Enabled: true, Mode: BackpropModeActive, Version: "v1", GroundTruth: optOut},
		},
		{
			name:     "explicit-mode workflow ground truth overrides gaggle opt-out",
			workflow: &BackpropConfig{Mode: BackpropModeActive, Version: "v1", GroundTruth: humanOnly}, gaggle: &GaggleBackprop{Mode: BackpropModeActive, GroundTruth: optOut},
			want: BackpropConfig{Mode: BackpropModeActive, Version: "v1", GroundTruth: humanOnly},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ResolveBackprop(test.workflow, test.gaggle); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("ResolveBackprop() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestResolveBackpropDoesNotAliasGroundTruth(t *testing.T) {
	gaggle := &GaggleBackprop{Mode: BackpropModeActive, GroundTruth: &BackpropGroundTruth{Sources: []BackpropLabelSource{BackpropLabelSourceHuman}}}
	resolved := ResolveBackprop(nil, gaggle)
	resolved.GroundTruth.Sources[0] = "mutated"
	if gaggle.GroundTruth.Sources[0] != BackpropLabelSourceHuman {
		t.Fatal("resolved config aliases the gaggle ground-truth sources")
	}
}

func TestBackpropGroundTruthAcceptsLabelSource(t *testing.T) {
	var unset *BackpropGroundTruth
	if !unset.AcceptsLabelSource(BackpropLabelSourceHuman) {
		t.Fatal("an omitted contract must accept every supported source")
	}
	if (&BackpropGroundTruth{Sources: []BackpropLabelSource{}}).AcceptsLabelSource(BackpropLabelSourceHuman) {
		t.Fatal("an empty source list must opt out")
	}
}
