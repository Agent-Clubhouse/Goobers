package workflow

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	vcurrent "github.com/goobers/goobers/internal/workflow/v_current"
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

// LookupFeature returns support metadata from the current interpreter.
func LookupFeature(id FeatureID) (Feature, bool) {
	return vcurrent.LookupFeature(id)
}

// AllFeatures returns a stable snapshot of current DSL features.
func AllFeatures() []Feature {
	return vcurrent.AllFeatures()
}

// FeaturesAtDSLVersion filters features to one DSL version.
func FeaturesAtDSLVersion(features []Feature, version string) ([]Feature, error) {
	return vcurrent.FeaturesAtDSLVersion(features, version)
}

// FeaturesForWorkflow resolves features used by a workflow definition.
func FeaturesForWorkflow(def Definition) ([]Feature, error) {
	return vcurrent.FeaturesForWorkflow(def)
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
	return vcurrent.CheckWorkflowFeatureSupport(def, allowPreview)
}

// CheckGooberFeatureSupport resolves a goober and applies support policy.
func CheckGooberFeatureSupport(spec apiv1.GooberSpec, allowPreview bool) []FeatureDiagnostic {
	return vcurrent.CheckGooberFeatureSupport(spec, allowPreview)
}
