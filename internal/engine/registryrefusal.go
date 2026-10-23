package engine

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engineadmission"
)

// Keep the engine API's sentinel identity and errors.As types while sharing
// the same fail-closed policy with child admission.
var (
	ErrExperimentUnsupported = engineadmission.ErrExperimentUnsupported
	ErrOutboxUnsupported     = engineadmission.ErrOutboxUnsupported
	ErrParallelsUnsupported  = engineadmission.ErrParallelsUnsupported
	ErrMapsUnsupported       = engineadmission.ErrMapsUnsupported
)

// UnsupportedFeatureError names a refused declaration and wraps its sentinel.
type UnsupportedFeatureError = engineadmission.UnsupportedFeatureError

// UnsupportedFeaturesError groups a workflow's refused declarations.
type UnsupportedFeaturesError = engineadmission.UnsupportedFeaturesError

func refuseHumanGate(g apiv1.Gate) error {
	return engineadmission.RefuseHumanGate(g)
}
