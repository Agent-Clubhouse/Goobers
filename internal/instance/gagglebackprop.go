package instance

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/supportmatrix"
	"github.com/goobers/goobers/internal/workflow"
)

// ApplyGaggleBackprop resolves each gaggle's Backprop defaults onto its
// workflows before compilation (#7120). A workflow whose effective config is
// active gets that config pinned into its definition, so the run journal
// records exactly what the terminal path attributes under. Shadow and off are
// left unpinned: shadow does no run-time work and is resolved on demand
// against the gaggle's current default, as before.
//
// Inheritance never reaches a workflow that cannot compile Backprop: one
// pinned below DSL 3.0 or without the preview-features acknowledgement keeps
// its own definition, and InheritedGaggleBackprop reports which defaults
// still apply to it on read surfaces.
func ApplyGaggleBackprop(set *ConfigSet) {
	if set == nil {
		return
	}
	defaults := make(map[string]*apiv1.GaggleBackprop, len(set.Gaggles))
	for i := range set.Gaggles {
		if set.Gaggles[i].Spec.Backprop != nil {
			defaults[set.Gaggles[i].Name] = set.Gaggles[i].Spec.Backprop
		}
	}
	if len(defaults) == 0 {
		return
	}
	for i := range set.Workflows {
		wf := &set.Workflows[i]
		gaggle, ok := defaults[wf.Spec.Gaggle]
		if !ok || !backpropInheritanceEligible(wf) {
			continue
		}
		resolved := apiv1.ResolveBackprop(wf.Spec.Backprop, gaggle)
		if resolved.Mode == apiv1.BackpropModeActive {
			wf.Spec.Backprop = &resolved
		}
	}
}

// InheritedGaggleBackprop returns the subset of a gaggle's Backprop defaults
// that applies to wf: all of them for a workflow that can compile Backprop,
// and otherwise only a shadow default, which is observed on demand from the
// run journal and needs nothing compiled.
func InheritedGaggleBackprop(wf *apiv1.Workflow, gaggle *apiv1.GaggleBackprop) *apiv1.GaggleBackprop {
	if gaggle == nil || wf == nil || backpropInheritanceEligible(wf) {
		return gaggle
	}
	if gaggle.Mode == apiv1.BackpropModeShadow {
		return gaggle
	}
	return nil
}

func backpropInheritanceEligible(wf *apiv1.Workflow) bool {
	order, ok := supportmatrix.CompareDSLVersions(wf.DSLVersion, supportmatrix.V3DSLVersion)
	return ok && order >= 0 && workflow.PreviewFeaturesEnabled(wf.Annotations)
}
