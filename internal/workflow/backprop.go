package workflow

import (
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// validateBackpropMode rejects unknown modes and the one contradictory
// combination: enabled=true alongside an explicit mode=off.
func validateBackpropMode(config *apiv1.BackpropConfig) error {
	if config == nil {
		return nil
	}
	switch config.Mode {
	case "", apiv1.BackpropModeShadow, apiv1.BackpropModeActive:
		return nil
	case apiv1.BackpropModeOff:
		if config.Enabled {
			return fmt.Errorf("backprop.enabled=true contradicts backprop.mode=%q; remove enabled or choose shadow or active", config.Mode)
		}
		return nil
	default:
		return fmt.Errorf("backprop.mode %q must be off, shadow, or active", config.Mode)
	}
}
