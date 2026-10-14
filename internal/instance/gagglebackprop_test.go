package instance

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow"
)

func backpropWorkflow(name, dsl string, preview bool, config *apiv1.BackpropConfig) apiv1.Workflow {
	wf := apiv1.Workflow{Spec: apiv1.WorkflowSpec{Gaggle: "web", Backprop: config}}
	wf.Name = name
	wf.DSLVersion = dsl
	if preview {
		wf.Annotations = map[string]string{workflow.PreviewFeaturesAnnotation: "true"}
	}
	return wf
}

func TestApplyGaggleBackpropInheritance(t *testing.T) {
	active := &apiv1.GaggleBackprop{Mode: apiv1.BackpropModeActive, Granularity: apiv1.BackpropGranularityStage}
	tests := []struct {
		name     string
		gaggle   *apiv1.GaggleBackprop
		workflow apiv1.Workflow
		want     *apiv1.BackpropConfig
	}{
		{
			name:     "omitted inherits active defaults",
			gaggle:   active,
			workflow: backpropWorkflow("impl", "3.0", true, nil),
			want:     &apiv1.BackpropConfig{Enabled: true, Mode: apiv1.BackpropModeActive, Version: "v1", Granularity: apiv1.BackpropGranularityStage},
		},
		{
			name:     "enabled false without mode inherits",
			gaggle:   active,
			workflow: backpropWorkflow("impl", "3.0", true, &apiv1.BackpropConfig{Version: "v1"}),
			want:     &apiv1.BackpropConfig{Enabled: true, Mode: apiv1.BackpropModeActive, Version: "v1", Granularity: apiv1.BackpropGranularityStage},
		},
		{
			name:     "inherited mode keeps workflow granularity",
			gaggle:   active,
			workflow: backpropWorkflow("impl", "3.0", true, &apiv1.BackpropConfig{Version: "v1", Granularity: apiv1.BackpropGranularityRun}),
			want:     &apiv1.BackpropConfig{Enabled: true, Mode: apiv1.BackpropModeActive, Version: "v1", Granularity: apiv1.BackpropGranularityRun},
		},
		{
			name:     "explicit off disables",
			gaggle:   active,
			workflow: backpropWorkflow("impl", "3.0", true, &apiv1.BackpropConfig{Mode: apiv1.BackpropModeOff, Version: "v1"}),
			want:     &apiv1.BackpropConfig{Mode: apiv1.BackpropModeOff, Version: "v1"},
		},
		{
			name:     "workflow shadow overrides gaggle active",
			gaggle:   active,
			workflow: backpropWorkflow("impl", "3.0", true, &apiv1.BackpropConfig{Mode: apiv1.BackpropModeShadow, Version: "v1"}),
			want:     &apiv1.BackpropConfig{Mode: apiv1.BackpropModeShadow, Version: "v1"},
		},
		{
			name:     "workflow granularity overrides gaggle granularity",
			gaggle:   active,
			workflow: backpropWorkflow("impl", "3.0", true, &apiv1.BackpropConfig{Mode: apiv1.BackpropModeActive, Version: "v1", Granularity: apiv1.BackpropGranularityRun}),
			want:     &apiv1.BackpropConfig{Mode: apiv1.BackpropModeActive, Version: "v1", Granularity: apiv1.BackpropGranularityRun},
		},
		{
			name:     "workflow active inherits omitted granularity",
			gaggle:   active,
			workflow: backpropWorkflow("impl", "3.0", true, &apiv1.BackpropConfig{Mode: apiv1.BackpropModeActive, Version: "v1"}),
			want:     &apiv1.BackpropConfig{Mode: apiv1.BackpropModeActive, Version: "v1", Granularity: apiv1.BackpropGranularityStage},
		},
		{
			name:     "gaggle shadow stays read-time",
			gaggle:   &apiv1.GaggleBackprop{Mode: apiv1.BackpropModeShadow},
			workflow: backpropWorkflow("impl", "3.0", true, nil),
		},
		{
			name:     "below DSL 3.0 is not materialized",
			gaggle:   active,
			workflow: backpropWorkflow("impl", "2.0", true, nil),
		},
		{
			name:     "without preview acknowledgement is not materialized",
			gaggle:   active,
			workflow: backpropWorkflow("impl", "3.0", false, nil),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			set := &ConfigSet{
				Gaggles:   []apiv1.Gaggle{gaggle("web", apiv1.GaggleSpec{Backprop: test.gaggle})},
				Workflows: []apiv1.Workflow{test.workflow},
			}
			ApplyGaggleDefaults(set)
			got := set.Workflows[0].Spec.Backprop
			if (got == nil) != (test.want == nil) || (got != nil && *got != *test.want) {
				t.Fatalf("backprop = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestApplyGaggleBackpropLeavesOtherGagglesAlone(t *testing.T) {
	other := backpropWorkflow("impl", "3.0", true, nil)
	other.Spec.Gaggle = "api"
	set := &ConfigSet{
		Gaggles:   []apiv1.Gaggle{gaggle("web", apiv1.GaggleSpec{Backprop: &apiv1.GaggleBackprop{Mode: apiv1.BackpropModeActive}})},
		Workflows: []apiv1.Workflow{other},
	}
	ApplyGaggleBackprop(set)
	if set.Workflows[0].Spec.Backprop != nil {
		t.Fatalf("workflow in another gaggle inherited %+v", set.Workflows[0].Spec.Backprop)
	}
}

func TestInheritedGaggleBackpropForIneligibleWorkflow(t *testing.T) {
	legacy := backpropWorkflow("impl", "2.0", false, nil)
	if got := InheritedGaggleBackprop(&legacy, &apiv1.GaggleBackprop{Mode: apiv1.BackpropModeActive}); got != nil {
		t.Fatalf("active default reached an ineligible workflow: %+v", got)
	}
	shadow := &apiv1.GaggleBackprop{Mode: apiv1.BackpropModeShadow}
	if got := InheritedGaggleBackprop(&legacy, shadow); got != shadow {
		t.Fatalf("shadow default = %+v, want it to reach every workflow", got)
	}
	eligible := backpropWorkflow("impl", "3.0", true, nil)
	active := &apiv1.GaggleBackprop{Mode: apiv1.BackpropModeActive}
	if got := InheritedGaggleBackprop(&eligible, active); got != active {
		t.Fatalf("eligible workflow inherited %+v, want %+v", got, active)
	}
}
