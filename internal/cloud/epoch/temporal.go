package epoch

import (
	"context"
	"errors"
	"fmt"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
)

// Lister is the visibility call OpenRuns needs; client.Client satisfies it.
type Lister interface {
	ListWorkflow(ctx context.Context, request *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error)
}

// Describer is the strongly consistent per-run call; client.Client satisfies it.
type Describer interface {
	DescribeWorkflowExecution(ctx context.Context, workflowID, runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
}

// Temporal is the client surface the epoch needs.
type Temporal interface {
	Lister
	Describer
}

// TemporalConfirmClosed confirms a run is closed through
// DescribeWorkflowExecution (strongly consistent, unlike visibility): not found
// or a closed status is closed; running or paused is open; any other error is
// returned so the caller keeps the claim this tick. Same pattern as the worker
// sweep and the start fence.
func TemporalConfirmClosed(client Describer) func(ctx context.Context, runID string) (bool, error) {
	return func(ctx context.Context, runID string) (bool, error) {
		if client == nil {
			return false, errors.New("cloud epoch: no Temporal client to confirm run state")
		}
		response, err := client.DescribeWorkflowExecution(ctx, runID, "")
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		switch response.GetWorkflowExecutionInfo().GetStatus() {
		case enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, enumspb.WORKFLOW_EXECUTION_STATUS_PAUSED:
			return false, nil
		case enumspb.WORKFLOW_EXECUTION_STATUS_UNSPECIFIED:
			return false, fmt.Errorf("cloud epoch: run %q has no execution status", runID)
		default:
			return true, nil
		}
	}
}

// maxOpenRuns bounds one listing so a runaway visibility store cannot exhaust
// memory; exceeding it fails the listing (liveness cannot be proven).
const maxOpenRuns = 100_000

// TemporalOpenRuns lists running workflows through visibility. A workflow ID is
// the engine run ID (startfence uses RunInput.RunID), which is what claims name.
type TemporalOpenRuns struct{ Client Lister }

// OpenRuns pages every running workflow. Any page error fails the call: a
// partial set would let the reap release a live run's claim.
func (t TemporalOpenRuns) OpenRuns(ctx context.Context) (map[string]bool, error) {
	if t.Client == nil {
		return nil, errors.New("cloud epoch: no Temporal client to list open runs")
	}
	open := map[string]bool{}
	var token []byte
	for {
		response, err := t.Client.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
			Query: "ExecutionStatus = 'Running'", NextPageToken: token,
		})
		if err != nil {
			return nil, err
		}
		for _, info := range response.GetExecutions() {
			if id := info.GetExecution().GetWorkflowId(); id != "" {
				open[id] = true
			}
		}
		if len(open) > maxOpenRuns {
			return nil, errors.New("cloud epoch: open-run listing exceeds its bound")
		}
		token = response.GetNextPageToken()
		if len(token) == 0 {
			return open, nil
		}
	}
}
