package instance

import apiv1 "github.com/goobers/goobers/api/v1alpha1"

// CostReportingEnabled resolves gaggle override, instance default, then the
// built-in enabled default. It does not control local usage accounting.
func (c *Config) CostReportingEnabled(gaggle *apiv1.Gaggle) bool {
	if gaggle != nil && gaggle.Spec.Cost != nil && gaggle.Spec.Cost.Enabled != nil {
		return *gaggle.Spec.Cost.Enabled
	}
	if c != nil && c.Cost != nil && c.Cost.Enabled != nil {
		return *c.Cost.Enabled
	}
	return true
}
