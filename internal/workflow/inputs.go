package workflow

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	vcurrent "github.com/goobers/goobers/internal/workflow/v_current"
)

// TaskInvocationInputs returns the inputs a runner sends to a task.
func TaskInvocationInputs(machine *Machine, task apiv1.Task) map[string]string {
	return vcurrent.TaskInvocationInputs(machine, task)
}
