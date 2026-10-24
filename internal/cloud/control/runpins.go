package control

import (
	"context"
	"errors"
	"fmt"

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
// ID) has a closed status, or Temporal no longer knows it at all (closed and
// past Temporal's own retention). Running and paused runs are open. The closed
// set is enumerated: an unspecified or unrecognized status (a server upgrade
// adds an enum value, as Paused once was) is an error, never closed, so the
// sweep leaves the pin in place. Same rule as epoch.TemporalConfirmClosed
// (#8109), which this should call once it lands.
func temporalRunClosed(temporal workflowDescriber) acceptance.RunClosed {
	return func(ctx context.Context, runID string) (bool, error) {
		description, err := temporal.DescribeWorkflowExecution(ctx, runID, "")
		var notFound *serviceerror.NotFound
		switch {
		case errors.As(err, &notFound):
			return true, nil
		case isTemporalUnavailable(err):
			return false, fmt.Errorf("%w: %w", acceptance.ErrRunStatusUnavailable, err)
		case err != nil:
			return false, err
		case description.GetWorkflowExecutionInfo() == nil:
			return false, errors.New("control: run workflow description is incomplete")
		}
		switch status := description.GetWorkflowExecutionInfo().GetStatus(); status {
		case enums.WORKFLOW_EXECUTION_STATUS_RUNNING, enums.WORKFLOW_EXECUTION_STATUS_PAUSED:
			return false, nil
		case enums.WORKFLOW_EXECUTION_STATUS_COMPLETED, enums.WORKFLOW_EXECUTION_STATUS_FAILED,
			enums.WORKFLOW_EXECUTION_STATUS_CANCELED, enums.WORKFLOW_EXECUTION_STATUS_TERMINATED,
			enums.WORKFLOW_EXECUTION_STATUS_TIMED_OUT, enums.WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW:
			return true, nil
		default:
			return false, fmt.Errorf("control: run %q has unrecognized execution status %d", runID, int32(status))
		}
	}
}

// isTemporalUnavailable reports an error that says nothing about the run and
// everything about Temporal: the sweep stops the pass rather than wait out the
// same timeout on every remaining candidate.
func isTemporalUnavailable(err error) bool {
	var unavailable *serviceerror.Unavailable
	var deadline *serviceerror.DeadlineExceeded
	return errors.As(err, &unavailable) || errors.As(err, &deadline) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}
