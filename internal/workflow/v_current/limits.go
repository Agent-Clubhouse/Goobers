package vcurrent

import apiv1 "github.com/goobers/goobers/api/v1alpha1"

// TaskLimits returns the wire limits compiled from a task's execution policy.
func TaskLimits(task apiv1.Task) apiv1.Limits {
	var limits apiv1.Limits
	if task.Limits != nil {
		limits = *task.Limits
	}
	if task.TimeoutSeconds > 0 {
		limits.MaxDurationSeconds = task.TimeoutSeconds
	}
	return limits
}

// GateLimits returns the wire limits declared by the selected gate evaluator.
func GateLimits(gate apiv1.Gate) apiv1.Limits {
	var timeout int32
	switch gate.Evaluator {
	case apiv1.EvaluatorAutomated:
		if gate.Automated != nil {
			timeout = gate.Automated.TimeoutSeconds
		}
	case apiv1.EvaluatorAgentic:
		if gate.Agentic != nil {
			timeout = gate.Agentic.TimeoutSeconds
		}
	}
	return apiv1.Limits{MaxDurationSeconds: timeout}
}
