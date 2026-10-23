package control

import (
	"context"
	"errors"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"

	"github.com/goobers/goobers/internal/cloud/acceptance"
)

// workflowDescriber is the slice of the Temporal client the run-pin sweep needs.
type workflowDescriber interface {
	DescribeWorkflowExecution(context.Context, string, string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
}

// temporalRunClosed reports a run closed when its engine workflow (ID = run
// ID) is no longer running, or Temporal no longer knows it at all (closed and
// past Temporal's own retention). Any other error is returned, so the sweep
// leaves the pin in place.
func temporalRunClosed(temporal workflowDescriber) acceptance.RunClosed {
	return func(ctx context.Context, runID string) (bool, error) {
		description, err := temporal.DescribeWorkflowExecution(ctx, runID, "")
		var notFound *serviceerror.NotFound
		switch {
		case errors.As(err, &notFound):
			return true, nil
		case err != nil:
			return false, err
		case description.GetWorkflowExecutionInfo() == nil:
			return false, errors.New("control: run workflow description is incomplete")
		default:
			return description.GetWorkflowExecutionInfo().GetStatus() != enums.WORKFLOW_EXECUTION_STATUS_RUNNING, nil
		}
	}
}
