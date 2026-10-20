package engine

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engineadmission"
)

// Keep the engine API's sentinel identity and errors.As types while sharing
// the same fail-closed policy with child admission. No refusal is lifted.
var (
	ErrExperimentUnsupported        = engineadmission.ErrExperimentUnsupported
	ErrRemoteUsageLimitsUnsupported = engineadmission.ErrRemoteUsageLimitsUnsupported
	ErrOutboxUnsupported            = engineadmission.ErrOutboxUnsupported
	ErrParallelsUnsupported         = engineadmission.ErrParallelsUnsupported
	ErrSelectiveParallelUnsupported = engineadmission.ErrSelectiveParallelUnsupported
	ErrMapsUnsupported              = engineadmission.ErrMapsUnsupported
)

type UnsupportedFeatureError = engineadmission.UnsupportedFeatureError
type UnsupportedFeaturesError = engineadmission.UnsupportedFeaturesError

func unsupportedEngineFeatures(spec apiv1.WorkflowSpec) []*UnsupportedFeatureError {
	return engineadmission.UnsupportedFeatures(spec)
}

func refuseUnsupportedEngineFeatures(name string, spec apiv1.WorkflowSpec) error {
	return engineadmission.RefuseFeatures(name, spec)
}

func refuseHumanGate(g apiv1.Gate) error {
	return engineadmission.RefuseHumanGate(g)
}

// RefuseDefinition checks the engine's supported-feature boundary before start.
func RefuseDefinition(name string, spec apiv1.WorkflowSpec) error {
	return engineadmission.RefuseDefinition(name, spec)
}

// RefusePlacedDefinition also checks restrictions that depend on placement.
func RefusePlacedDefinition(name string, spec apiv1.WorkflowSpec, placements []PinnedPlacement) error {
	return engineadmission.RefusePlacedDefinition(name, spec, placements)
}

func refuseRemoteUsage(name string, spec apiv1.WorkflowSpec, placements []PinnedPlacement) error {
	return engineadmission.RefuseRemoteUsage(name, spec, placements)
}
