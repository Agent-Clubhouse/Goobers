package workflow

import apiv1 "github.com/goobers/goobers/api/v1alpha1"

// FeatureInteractive is additive preview configuration, independent of workflow
// interpreter features and of DSL 3.1 task.childWorkflows.
const FeatureInteractive FeatureID = "gaggle.spec.interactive"

func interactiveFeature() Feature {
	return Feature{ID: FeatureInteractive, Level: SupportPreview, SinceVersion: "dev",
		History:     []SupportTransition{{Level: SupportPreview, SinceVersion: "dev"}},
		DSLVersions: []DSLFeatureSupport{{Version: "3.0", Level: SupportPreview}}}
}

// CheckInteractiveFeatureSupport uses only this Gaggle's interactive pin and
// acknowledgement. It neither inherits Manifest acknowledgement nor authorizes
// any Workflow's preview version. Existing workflow fan-out stays unchanged.
func CheckInteractiveFeatureSupport(g apiv1.Gaggle) []FeatureDiagnostic {
	if g.Spec.Interactive == nil {
		return nil
	}
	return CheckFeatureSupport(Definition{DSLVersion: g.DSLVersion}, []Feature{interactiveFeature()}, PreviewFeaturesEnabled(g.Annotations))
}
