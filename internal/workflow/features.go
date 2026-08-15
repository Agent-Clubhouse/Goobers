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

// NewFeatureRegistry validates and copies feature entries for a pinned definition.
func NewFeatureRegistry(def Definition, features []Feature) (FeatureRegistry, error) {
	interpreter, err := interpreterForDefinition(def)
	if err != nil {
		return FeatureRegistry{}, err
	}
	return interpreter.newFeatureRegistry(features)
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
	interpreter, err := interpreterForDefinition(Definition{DSLVersion: version})
	if err != nil {
		return vcurrent.FeaturesAtDSLVersion(features, version)
	}
	return interpreter.featuresAtDSLVersion(features, version)
}

// FeaturesForWorkflow resolves features used by a workflow definition.
func FeaturesForWorkflow(def Definition) ([]Feature, error) {
	interpreter, err := interpreterForDefinition(def)
	if err != nil {
		return nil, err
	}
	return interpreter.featuresForWorkflow(def)
}

// FeaturesForGaggle resolves features used by a gaggle for a pinned definition.
func FeaturesForGaggle(def Definition, spec apiv1.GaggleSpec) ([]Feature, error) {
	interpreter, err := interpreterForDefinition(def)
	if err != nil {
		return nil, err
	}
	return interpreter.featuresForGaggle(spec)
}

// FeaturesForGoober resolves features used by a goober for a pinned definition.
func FeaturesForGoober(def Definition, spec apiv1.GooberSpec) ([]Feature, error) {
	interpreter, err := interpreterForDefinition(def)
	if err != nil {
		return nil, err
	}
	return interpreter.featuresForGoober(spec)
}

// CheckFeatureSupport applies the pinned definition's support policy.
func CheckFeatureSupport(def Definition, features []Feature, allowPreview bool) []FeatureDiagnostic {
	interpreter, err := interpreterForDefinition(def)
	if err != nil {
		return []FeatureDiagnostic{{Blocking: true, Message: err.Error()}}
	}
	return interpreter.checkFeatureSupport(features, allowPreview)
}

// CheckWorkflowFeatureSupport resolves a workflow and applies support policy.
func CheckWorkflowFeatureSupport(def Definition, allowPreview bool) []FeatureDiagnostic {
	interpreter, err := interpreterForDefinition(def)
	if err != nil {
		return []FeatureDiagnostic{{Blocking: true, Message: err.Error()}}
	}
	return interpreter.checkWorkflowFeatureSupport(def, allowPreview)
}

// CheckGaggleFeatureSupport resolves a gaggle and applies support policy.
func CheckGaggleFeatureSupport(def Definition, spec apiv1.GaggleSpec, allowPreview bool) []FeatureDiagnostic {
	features, err := FeaturesForGaggle(def, spec)
	if err != nil {
		return []FeatureDiagnostic{{Blocking: true, Message: err.Error()}}
	}
	return CheckFeatureSupport(def, features, allowPreview)
}

// CheckGooberFeatureSupport resolves a goober and applies support policy.
func CheckGooberFeatureSupport(def Definition, spec apiv1.GooberSpec, allowPreview bool) []FeatureDiagnostic {
	features, err := FeaturesForGoober(def, spec)
	if err != nil {
		return []FeatureDiagnostic{{Blocking: true, Message: err.Error()}}
	}
	return CheckFeatureSupport(def, features, allowPreview)
}

func newNextFeatureRegistry(features []Feature) (FeatureRegistry, error) {
	next := make([]vnext.Feature, len(features))
	for i, feature := range features {
		next[i] = featureForNext(feature)
	}
	if _, err := vnext.NewFeatureRegistry(next); err != nil {
		return FeatureRegistry{}, err
	}
	return vcurrent.NewFeatureRegistry(features)
}

func nextFeaturesAtDSLVersion(features []Feature, version string) ([]Feature, error) {
	next := make([]vnext.Feature, len(features))
	for i, feature := range features {
		next[i] = featureForNext(feature)
	}
	filtered, err := vnext.FeaturesAtDSLVersion(next, version)
	if err != nil {
		return nil, err
	}
	return featuresFromNext(filtered), nil
}

func featuresForNextWorkflow(def Definition) ([]Feature, error) {
	features, err := vnext.FeaturesForWorkflow(def)
	if err != nil {
		return nil, err
	}
	return featuresFromNext(features), nil
}

func featuresForNextGaggle(spec apiv1.GaggleSpec) ([]Feature, error) {
	features, err := vnext.FeaturesForGaggle(spec)
	if err != nil {
		return nil, err
	}
	return featuresFromNext(features), nil
}

func featuresForNextGoober(spec apiv1.GooberSpec) ([]Feature, error) {
	features, err := vnext.FeaturesForGoober(spec)
	if err != nil {
		return nil, err
	}
	return featuresFromNext(features), nil
}

func checkNextFeatureSupport(features []Feature, allowPreview bool) []FeatureDiagnostic {
	next := make([]vnext.Feature, len(features))
	for i, feature := range features {
		next[i] = featureForNext(feature)
	}
	return diagnosticsFromNext(vnext.CheckFeatureSupport(next, allowPreview))
}

func checkNextWorkflowFeatureSupport(def Definition, allowPreview bool) []FeatureDiagnostic {
	return diagnosticsFromNext(vnext.CheckWorkflowFeatureSupport(def, allowPreview))
}

func featuresFromNext(features []vnext.Feature) []Feature {
	out := make([]Feature, len(features))
	for i, feature := range features {
		out[i] = nextFeature(feature)
	}
	return out
}

func diagnosticsFromNext(diagnostics []vnext.FeatureDiagnostic) []FeatureDiagnostic {
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

func featureForNext(feature Feature) vnext.Feature {
	out := vnext.Feature{
		ID:                    vnext.FeatureID(feature.ID),
		Level:                 vnext.SupportLevel(feature.Level),
		SinceVersion:          feature.SinceVersion,
		Replacement:           vnext.FeatureID(feature.Replacement),
		RemovalTargetVersion:  feature.RemovalTargetVersion,
		LastSupportingVersion: feature.LastSupportingVersion,
		DSLVersions:           make([]vnext.DSLFeatureSupport, len(feature.DSLVersions)),
		History:               make([]vnext.SupportTransition, len(feature.History)),
	}
	for i, support := range feature.DSLVersions {
		out.DSLVersions[i] = vnext.DSLFeatureSupport{
			Version: support.Version,
			Level:   vnext.SupportLevel(support.Level),
		}
	}
	for i, transition := range feature.History {
		out.History[i] = vnext.SupportTransition{
			Level:        vnext.SupportLevel(transition.Level),
			SinceVersion: transition.SinceVersion,
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
