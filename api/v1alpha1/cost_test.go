package v1alpha1

import "testing"

func TestGaggleCostDeepCopy(t *testing.T) {
	enabled := false
	original := &Gaggle{Spec: GaggleSpec{Cost: &CostReporting{Enabled: &enabled}}}
	copy := original.DeepCopy()
	*copy.Spec.Cost.Enabled = true
	if *original.Spec.Cost.Enabled {
		t.Fatal("copy aliases the source cost override")
	}
	for _, setting := range []*CostReporting{nil, {}} {
		original.Spec.Cost = setting
		copy = original.DeepCopy()
		if (copy.Spec.Cost == nil) != (setting == nil) {
			t.Fatal("copy changed omitted cost configuration")
		}
		if copy.Spec.Cost != nil && copy.Spec.Cost.Enabled != nil {
			t.Fatal("copy defaulted an inherited override")
		}
	}
}
