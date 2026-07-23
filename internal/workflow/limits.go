package workflow

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	vcurrent "github.com/goobers/goobers/internal/workflow/v_current"
)

// TaskLimits returns the wire limits compiled from a task policy.
func TaskLimits(task apiv1.Task) apiv1.Limits {
	return vcurrent.TaskLimits(task)
}

// GateLimits returns the wire limits declared by a gate evaluator.
func GateLimits(gate apiv1.Gate) apiv1.Limits {
	return vcurrent.GateLimits(gate)
}
