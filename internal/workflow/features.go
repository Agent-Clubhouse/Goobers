package workflow

import (
	"slices"
	"sort"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/supportmatrix"
	v30 "github.com/goobers/goobers/internal/workflow/v_3_0"
	vnext "github.com/goobers/goobers/internal/workflow/v_next"
)

// The shared feature types are re-exported from the oldest live interpreter,
// which is DSL 2.0 (v_next) now that 1.4 (v_current) is dropped (#3507). Newer
// interpreters' feature values are converted into this canonical shape by the
// featureForNext/v30 glue below.

// FeatureID is the stable, author-facing name of a DSL capability.
type FeatureID = vnext.FeatureID

// SupportLevel describes the compatibility promise for a DSL feature.
type SupportLevel = vnext.SupportLevel

// SupportTransition records when a feature entered a support level.
type SupportTransition = vnext.SupportTransition

// Feature records a DSL feature's support metadata.
type Feature = vnext.Feature

// DSLFeatureSupport records a feature's support in one DSL version.
type DSLFeatureSupport = vnext.DSLFeatureSupport

// FeatureRegistry is an immutable feature-support lookup table.
type FeatureRegistry struct {
	entries map[FeatureID]Feature
}

// FeatureDiagnostic describes one support-level finding.
type FeatureDiagnostic = vnext.FeatureDiagnostic

const (
	// SupportPreview marks an unstable feature requiring acknowledgement.
	SupportPreview = vnext.SupportPreview
	// SupportGA marks a stable feature.
	SupportGA = vnext.SupportGA
	// SupportDeprecated marks a feature scheduled for removal.
	SupportDeprecated = vnext.SupportDeprecated
	// SupportRemoved marks a feature validation rejects.
	SupportRemoved = vnext.SupportRemoved
)

// NewFeatureRegistry validates and copies feature entries for a pinned definition.
func NewFeatureRegistry(def Definition, features []Feature) (FeatureRegistry, error) {
	interpreter, err := interpreterForDefinition(def)
	if err != nil {
		return FeatureRegistry{}, err
	}
	return interpreter.newFeatureRegistry(features)
}

// Lookup returns the support metadata for id.
func (r FeatureRegistry) Lookup(id FeatureID) (Feature, bool) {
	feature, ok := r.entries[id]
	return cloneFeature(feature), ok
}

// All returns every feature in stable ID order.
func (r FeatureRegistry) All() []Feature {
	features := make([]Feature, 0, len(r.entries))
	for _, feature := range r.entries {
		features = append(features, cloneFeature(feature))
	}
	sort.Slice(features, func(i, j int) bool {
		return features[i].ID < features[j].ID
	})
	return features
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
	// v_next (DSL 2.0) is the oldest live interpreter and supplies the
	// canonical Feature type, so its features are the base with no conversion.
	features := vnext.AllFeatures()
	byID := make(map[FeatureID]int, len(features))
	for i, feature := range features {
		byID[feature.ID] = i
	}
	merge := func(converted Feature) {
		i, ok := byID[converted.ID]
		if !ok {
			byID[converted.ID] = len(features)
			features = append(features, converted)
			return
		}
		features[i].DSLVersions = append(features[i].DSLVersions, converted.DSLVersions...)
	}
	for _, feature := range v30.AllFeatures() {
		merge(v30Feature(feature))
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
		return vnext.FeaturesAtDSLVersion(features, version)
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

// FeatureDefinitionsByDSLVersion collapses workflow definitions into one
// feature-probe Definition per distinct DSL pin, in ascending version order.
// Gaggle/goober feature-support checks fan out over the returned probes: those
// objects carry no dslVersion of their own, so their effective DSL version is
// the set of pins their workflows declare, and a scoped feature must be
// supported at every pin present (docs/design/dsl-version-lifecycle.md §8.4).
//
// An empty input — a gaggle or goober with no workflows — resolves at the
// newest LevelSupported version from the support matrix, never as an unpinned
// Definition{}. The version router rewrites an unpinned probe to
// CurrentDSLVersion, which is deprecated; the moment it turns unsupported,
// every workflow-less object would fail validation with a migrate-your-pin
// error its author cannot act on, because those specs deliberately have no
// dslVersion field to edit (#3297).
func FeatureDefinitionsByDSLVersion(definitions []Definition) []Definition {
	byVersion := map[string]Definition{}
	for _, definition := range definitions {
		byVersion[definition.DSLVersion] = Definition{
			Name: definition.Name, DSLVersion: definition.DSLVersion, Spec: definition.Spec,
		}
	}
	if len(byVersion) == 0 {
		version, ok := supportmatrix.GetDSL().NewestSupported()
		if !ok {
			// No supported version declared at all — a matrix state the support
			// policy rejects. Fall back to the transitional default rather than
			// fabricate a version the router would refuse to route.
			version = supportmatrix.CurrentDSLVersion
		}
		return []Definition{{DSLVersion: version}}
	}
	versions := make([]string, 0, len(byVersion))
	for version := range byVersion {
		versions = append(versions, version)
	}
	sort.Strings(versions)
	out := make([]Definition, 0, len(versions))
	for _, version := range versions {
		out = append(out, byVersion[version])
	}
	return out
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
	return newNextFeatureRegistryWith(features, vnext.NewFeatureRegistry)
}

func newNextFeatureRegistryWith(
	features []Feature,
	validate func([]vnext.Feature) (vnext.FeatureRegistry, error),
) (FeatureRegistry, error) {
	next := make([]vnext.Feature, len(features))
	for i, feature := range features {
		next[i] = featureForNext(feature)
	}
	validated, err := validate(next)
	if err != nil {
		return FeatureRegistry{}, err
	}
	return featureRegistry(featuresFromNext(validated.All())), nil
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

// --- DSL 3.0 glue: the same shape as the vnext conversions above. ---------

func newV30FeatureRegistry(features []Feature) (FeatureRegistry, error) {
	converted := make([]v30.Feature, len(features))
	for i, feature := range features {
		converted[i] = featureForV30(feature)
	}
	validated, err := v30.NewFeatureRegistry(converted)
	if err != nil {
		return FeatureRegistry{}, err
	}
	return featureRegistry(featuresFromV30(validated.All())), nil
}

func v30FeaturesAtDSLVersion(features []Feature, version string) ([]Feature, error) {
	converted := make([]v30.Feature, len(features))
	for i, feature := range features {
		converted[i] = featureForV30(feature)
	}
	filtered, err := v30.FeaturesAtDSLVersion(converted, version)
	if err != nil {
		return nil, err
	}
	return featuresFromV30(filtered), nil
}

func featuresForV30Workflow(def Definition) ([]Feature, error) {
	features, err := v30.FeaturesForWorkflow(def)
	if err != nil {
		return nil, err
	}
	return featuresFromV30(features), nil
}

func featuresForV30Gaggle(spec apiv1.GaggleSpec) ([]Feature, error) {
	features, err := v30.FeaturesForGaggle(spec)
	if err != nil {
		return nil, err
	}
	return featuresFromV30(features), nil
}

func featuresForV30Goober(spec apiv1.GooberSpec) ([]Feature, error) {
	features, err := v30.FeaturesForGoober(spec)
	if err != nil {
		return nil, err
	}
	return featuresFromV30(features), nil
}

func checkV30FeatureSupport(features []Feature, allowPreview bool) []FeatureDiagnostic {
	converted := make([]v30.Feature, len(features))
	for i, feature := range features {
		converted[i] = featureForV30(feature)
	}
	return diagnosticsFromV30(v30.CheckFeatureSupport(converted, allowPreview))
}

func checkV30WorkflowFeatureSupport(def Definition, allowPreview bool) []FeatureDiagnostic {
	return diagnosticsFromV30(v30.CheckWorkflowFeatureSupport(def, allowPreview))
}

func featuresFromV30(features []v30.Feature) []Feature {
	out := make([]Feature, len(features))
	for i, feature := range features {
		out[i] = v30Feature(feature)
	}
	return out
}

func diagnosticsFromV30(diagnostics []v30.FeatureDiagnostic) []FeatureDiagnostic {
	out := make([]FeatureDiagnostic, len(diagnostics))
	for i, diagnostic := range diagnostics {
		out[i] = FeatureDiagnostic{
			Feature:  v30Feature(diagnostic.Feature),
			Blocking: diagnostic.Blocking,
			Message:  diagnostic.Message,
		}
	}
	return out
}

func featureForV30(feature Feature) v30.Feature {
	out := v30.Feature{
		ID:                    v30.FeatureID(feature.ID),
		Level:                 v30.SupportLevel(feature.Level),
		SinceVersion:          feature.SinceVersion,
		Replacement:           v30.FeatureID(feature.Replacement),
		RemovalTargetVersion:  feature.RemovalTargetVersion,
		LastSupportingVersion: feature.LastSupportingVersion,
		DSLVersions:           make([]v30.DSLFeatureSupport, len(feature.DSLVersions)),
		History:               make([]v30.SupportTransition, len(feature.History)),
	}
	for i, support := range feature.DSLVersions {
		out.DSLVersions[i] = v30.DSLFeatureSupport{
			Version: support.Version,
			Level:   v30.SupportLevel(support.Level),
		}
	}
	for i, transition := range feature.History {
		out.History[i] = v30.SupportTransition{
			Level:        v30.SupportLevel(transition.Level),
			SinceVersion: transition.SinceVersion,
		}
	}
	return out
}

func v30Feature(feature v30.Feature) Feature {
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

func featureRegistry(features []Feature) FeatureRegistry {
	entries := make(map[FeatureID]Feature, len(features))
	for _, feature := range features {
		entries[feature.ID] = cloneFeature(feature)
	}
	return FeatureRegistry{entries: entries}
}

func cloneFeature(feature Feature) Feature {
	feature.History = slices.Clone(feature.History)
	feature.DSLVersions = slices.Clone(feature.DSLVersions)
	return feature
}
