package workflow

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TaskInvocationInputs returns the inputs a runner sends to a task.
func TaskInvocationInputs(machine *Machine, task apiv1.Task) (map[string]string, error) {
	interpreter, err := interpreterForMachine(machine)
	if err != nil {
		return nil, err
	}
	return interpreter.taskInvocationInputs(machine, task), nil
}
