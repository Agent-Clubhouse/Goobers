package runner

import (
	"fmt"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow"
)

func taskRetryPolicy(retry *apiv1.RetryPolicy) (int32, time.Duration) {
	attempts := int32(1)
	if retry == nil {
		return attempts, 0
	}
	if retry.MaxAttempts > 0 {
		attempts = retry.MaxAttempts
	}
	return attempts, time.Duration(retry.BackoffSeconds) * time.Second
}

func interruptedTaskBudgetResult(machine *workflow.Machine, task apiv1.Task) (*apiv1.ResultEnvelope, error) {
	if task.Type != apiv1.TaskAgentic {
		return nil, nil
	}
	limits, err := workflow.TaskLimits(machine, task)
	if err != nil {
		return nil, fmt.Errorf("project stage %q limits: %w", task.Name, err)
	}
	if !usageBudgetConfigured(limits) {
		return nil, nil
	}
	result := interruptedStageBudgetFailure(limits)
	return &result, nil
}
