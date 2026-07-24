package workflow

import (
	"sort"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	vcurrent "github.com/goobers/goobers/internal/workflow/v_current"
	vnext "github.com/goobers/goobers/internal/workflow/v_next"
)

// FeatureID is the stable, author-facing name of a DSL capability.
type FeatureID = vcurrent.FeatureID

// SupportLevel describes the compatibility promise for a DSL feature.
type SupportLevel = vcurrent.SupportLevel

// SupportTransition records when a feature entered a support level.
type SupportTransition = vcurrent.SupportTransition

// Feature records a DSL feature's support metadata.
type Feature = vcurrent.Feature

// DSLFeatureSupport records a feature's support in one DSL version.
type DSLFeatureSupport = vcurrent.DSLFeatureSupport

// FeatureRegistry is an immutable feature-support lookup table.
type FeatureRegistry = vcurrent.FeatureRegistry

// FeatureDiagnostic describes one support-level finding.
type FeatureDiagnostic = vcurrent.FeatureDiagnostic

const (
	// SupportPreview marks an unstable feature requiring acknowledgement.
	SupportPreview = vcurrent.SupportPreview
	// SupportGA marks a stable feature.
	SupportGA = vcurrent.SupportGA
	// SupportDeprecated marks a feature scheduled for removal.
	SupportDeprecated = vcurrent.SupportDeprecated
	// SupportRemoved marks a feature validation rejects.
	SupportRemoved = vcurrent.SupportRemoved
)

// NewFeatureRegistry validates and copies feature entries.
func NewFeatureRegistry(features []Feature) (FeatureRegistry, error) {
	return vcurrent.NewFeatureRegistry(features)
}

// LookupFeature returns support metadata across registered interpreters.
func LookupFeature(id FeatureID) (Feature, bool) {
	for _, feature := range AllFeatures() {
		if feature.ID == id {
			return feature, true
		}
	}
	return Feature{}, false
}

// AllFeatures returns a stable snapshot of features across registered DSL
// interpreters.
func AllFeatures() []Feature {
	features := vcurrent.AllFeatures()
	byID := make(map[FeatureID]int, len(features))
	for i, feature := range features {
		byID[feature.ID] = i
	}
	for _, feature := range vnext.AllFeatures() {
		converted := nextFeature(feature)
		i, ok := byID[converted.ID]
		if !ok {
			byID[converted.ID] = len(features)
			features = append(features, converted)
			continue
		}
		features[i].DSLVersions = append(features[i].DSLVersions, converted.DSLVersions...)
	}
	sort.Slice(features, func(i, j int) bool {
		return features[i].ID < features[j].ID
	})
	return features
}

// FeaturesAtDSLVersion filters features to one DSL version.
func FeaturesAtDSLVersion(features []Feature, version string) ([]Feature, error) {
	return vcurrent.FeaturesAtDSLVersion(features, version)
}

// FeaturesForWorkflow resolves features used by a workflow definition.
func FeaturesForWorkflow(def Definition) ([]Feature, error) {
	interpreter, err := interpreterForDefinition(def)
	if err != nil {
		return nil, err
	}
	return interpreter.featuresForWorkflow(def)
}

// FeaturesForGoober resolves features used by a goober definition.
func FeaturesForGoober(spec apiv1.GooberSpec) ([]Feature, error) {
	return vcurrent.FeaturesForGoober(spec)
}

// CheckFeatureSupport applies support policy to resolved features.
func CheckFeatureSupport(features []Feature, allowPreview bool) []FeatureDiagnostic {
	return vcurrent.CheckFeatureSupport(features, allowPreview)
}

// CheckWorkflowFeatureSupport resolves a workflow and applies support policy.
func CheckWorkflowFeatureSupport(def Definition, allowPreview bool) []FeatureDiagnostic {
	interpreter, err := interpreterForDefinition(def)
	if err != nil {
		return []FeatureDiagnostic{{Blocking: true, Message: err.Error()}}
	}
	return interpreter.checkWorkflowFeatureSupport(def, allowPreview)
}

// CheckGooberFeatureSupport resolves a goober and applies support policy.
func CheckGooberFeatureSupport(spec apiv1.GooberSpec, allowPreview bool) []FeatureDiagnostic {
	return vcurrent.CheckGooberFeatureSupport(spec, allowPreview)
}

func featuresForNextWorkflow(def Definition) ([]Feature, error) {
	features, err := vnext.FeaturesForWorkflow(def)
	if err != nil {
		return nil, err
	}
	out := make([]Feature, len(features))
	for i, feature := range features {
		out[i] = nextFeature(feature)
	}
	return out, nil
}

func checkNextWorkflowFeatureSupport(def Definition, allowPreview bool) []FeatureDiagnostic {
	diagnostics := vnext.CheckWorkflowFeatureSupport(def, allowPreview)
	out := make([]FeatureDiagnostic, len(diagnostics))
	for i, diagnostic := range diagnostics {
		out[i] = FeatureDiagnostic{
			Feature:  nextFeature(diagnostic.Feature),
			Blocking: diagnostic.Blocking,
			Message:  diagnostic.Message,
		}
	}
	return out
}

func nextFeature(feature vnext.Feature) Feature {
	out := Feature{
		ID:                    FeatureID(feature.ID),
		Level:                 SupportLevel(feature.Level),
		SinceVersion:          feature.SinceVersion,
		Replacement:           FeatureID(feature.Replacement),
		RemovalTargetVersion:  feature.RemovalTargetVersion,
		LastSupportingVersion: feature.LastSupportingVersion,
		DSLVersions:           make([]DSLFeatureSupport, len(feature.DSLVersions)),
		History:               make([]SupportTransition, len(feature.History)),
	}
	for i, support := range feature.DSLVersions {
		out.DSLVersions[i] = DSLFeatureSupport{
			Version: support.Version,
			Level:   SupportLevel(support.Level),
		}
	}
	for i, transition := range feature.History {
		out.History[i] = SupportTransition{
			Level:        SupportLevel(transition.Level),
			SinceVersion: transition.SinceVersion,
		}
	}
	return out
}
