package main

import (
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/instance"
)

// Cleanup of accepted custody still requires an enabled current parent stage.
// Removing its child opt-in revokes execution, not ownership of prior results.
func childCurrentCustodyPolicy(cfg *instance.Config, source *instance.ConfigSet, parent childworkflow.ParentSelection) (childworkflow.AdmissionContext, error) {
	return childworkflow.CurrentCustodyPolicy(cfg, source, parent, admitChildValidationGoobers)
}
