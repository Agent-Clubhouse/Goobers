package vcurrent

import (
	"strconv"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

const (
	taskKindInput         = "kind"
	ciPollTaskKind        = "ci-poll"
	ciPollIntervalInput   = "pollIntervalSeconds"
	durationSecondsSuffix = "s"
)

// TaskInvocationInputs returns the task inputs that a runner sends over the
// invocation envelope. A ci-poll task consumes the cadence declared by its
// immediately downstream automated gate, where GT-020 defines that policy.
func TaskInvocationInputs(machine *Machine, task apiv1.Task) map[string]string {
	if task.Inputs[taskKindInput] != ciPollTaskKind {
		return task.Inputs
	}
	gate, ok := machine.Gate(task.Next)
	if !ok || gate.Evaluator != apiv1.EvaluatorAutomated || gate.Automated == nil || gate.Automated.PollIntervalSeconds <= 0 {
		return task.Inputs
	}
	inputs := make(map[string]string, len(task.Inputs)+1)
	for key, value := range task.Inputs {
		inputs[key] = value
	}
	inputs[ciPollIntervalInput] = strconv.FormatInt(int64(gate.Automated.PollIntervalSeconds), 10) + durationSecondsSuffix
	return inputs
}
