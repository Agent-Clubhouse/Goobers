package workflow

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TaskLimits returns the wire limits compiled from a task policy.
func TaskLimits(machine *Machine, task apiv1.Task) (apiv1.Limits, error) {
	interpreter, err := interpreterForMachine(machine)
	if err != nil {
		return apiv1.Limits{}, err
	}
	return interpreter.taskLimits(task), nil
}

// GateLimits returns the wire limits declared by a gate evaluator.
func GateLimits(machine *Machine, gate apiv1.Gate) (apiv1.Limits, error) {
	interpreter, err := interpreterForMachine(machine)
	if err != nil {
		return apiv1.Limits{}, err
	}
	return interpreter.gateLimits(gate), nil
}
