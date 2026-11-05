package terminalawait

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.temporal.io/api/workflowservice/v1"
)

// Lister is the visibility call closed-run discovery needs; client.Client
// satisfies it.
type Lister interface {
	ListWorkflow(ctx context.Context, request *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error)
}

// maxClosedRuns bounds one listing; exceeding it fails the call (the watermark
// then stays put and the next Tick retries) rather than silently truncating.
const maxClosedRuns = 100_000

// TemporalClosedRuns lists workflows closed after since through visibility. A
// workflow ID is the engine run ID. Any page error fails the call: a partial
// listing must not let the watermark advance past an unlisted run.
func TemporalClosedRuns(client Lister) func(ctx context.Context, since time.Time) ([]ClosedRun, error) {
	return func(ctx context.Context, since time.Time) ([]ClosedRun, error) {
		if client == nil {
			return nil, errors.New("terminal awaiter: no Temporal client to list closed runs")
		}
		query := fmt.Sprintf(`ExecutionStatus != "Running" AND CloseTime > %q`, since.UTC().Format(time.RFC3339Nano))
		var (
			closed []ClosedRun
			token  []byte
		)
		for {
			response, err := client.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{Query: query, NextPageToken: token})
			if err != nil {
				return nil, err
			}
			for _, info := range response.GetExecutions() {
				if len(closed) >= maxClosedRuns {
					return nil, fmt.Errorf("terminal awaiter: more than %d runs closed since %s", maxClosedRuns, since.UTC().Format(time.RFC3339))
				}
				closed = append(closed, ClosedRun{RunID: info.GetExecution().GetWorkflowId(), CloseTime: info.GetCloseTime().AsTime()})
			}
			if token = response.GetNextPageToken(); len(token) == 0 {
				return closed, nil
			}
		}
	}
}
