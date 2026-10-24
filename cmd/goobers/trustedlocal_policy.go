package main

import (
	"fmt"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/workflow"
)

func trustedLocalChildren(cfg *instance.Config) bool {
	return childworkflow.UsesTrustedLocalExecution(cfg)
}

// The existing DSL preview acknowledgement is the opt-in. Platform selection
// never upgrades Windows Job membership into strict workspace custody.
func trustedLocalParentActivation(annotations map[string]string, goos string) error {
	if !workflow.PreviewFeaturesEnabled(annotations) {
		return fmt.Errorf("%w: trusted-local parents require %s=true", workflow.ErrChildWorkflowExecutionUnsupported, workflow.PreviewFeaturesAnnotation)
	}
	if goos != "darwin" && goos != "linux" {
		return fmt.Errorf("%w: trusted-local workspace custody is unqualified on %s; Windows Job membership cannot prove writer termination", workflow.ErrChildWorkflowExecutionUnsupported, goos)
	}
	return nil
}
