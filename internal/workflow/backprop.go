package workflow

import (
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// validateBackpropMode rejects unknown modes and enabled=true alongside any
// mode other than active. Binaries that predate mode read only enabled, so a
// shadow spec that also sets enabled=true would be treated as active by them
// and could publish attribution.json into the filing pass.
func validateBackpropMode(config *apiv1.BackpropConfig) error {
	if config == nil {
		return nil
	}
	switch config.Mode {
	case "", apiv1.BackpropModeActive:
		return nil
	case apiv1.BackpropModeOff, apiv1.BackpropModeShadow:
		if config.Enabled {
			return fmt.Errorf("backprop.enabled=true contradicts backprop.mode=%q; remove enabled or choose mode=active", config.Mode)
		}
		return nil
	default:
		return fmt.Errorf("backprop.mode %q must be off, shadow, or active", config.Mode)
	}
}
