package epoch

import (
	"context"
	"errors"

	"go.temporal.io/api/workflowservice/v1"
)

// Lister is the visibility call OpenRuns needs; client.Client satisfies it.
type Lister interface {
	ListWorkflow(ctx context.Context, request *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error)
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
