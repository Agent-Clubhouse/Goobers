package instance

import apiv1 "github.com/goobers/goobers/api/v1alpha1"

// EffectiveCostEnabled resolves cost report publication from the gaggle
// override, then the instance default, then the product default (enabled).
func EffectiveCostEnabled(config Config, gaggle *apiv1.Gaggle) bool {
	if gaggle != nil && gaggle.Spec.Cost != nil && gaggle.Spec.Cost.Enabled != nil {
		return *gaggle.Spec.Cost.Enabled
	}
	if config.Cost != nil && config.Cost.Enabled != nil {
		return *config.Cost.Enabled
	}
	return true
}
