package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowservice "go.temporal.io/api/workflowservice/v1"
)

// workflowLivenessClient is the slice of the Temporal client the DS6 claim
// liveness probe needs. client.Client satisfies it; tests provide a fake.
type workflowLivenessClient interface {
	DescribeWorkflowExecution(ctx context.Context, workflowID, runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
}

// describeLivenessTimeout bounds one liveness describe so a wedged frontend
// degrades that probe call to "unknown" (fail-live at the renewal layer)
// instead of stalling the daemon's whole claim tick.
const describeLivenessTimeout = 15 * time.Second

// WorkflowLiveness reports whether the engine workflow for a Goobers run id
// is still open — the engine half of DS6's claim-renewal liveness signal
// (docs/design/distributed-state-and-coordination.md §6/§10). The engine
// starts every run's workflow with WorkflowID == RunID
// (TemporalStarter.Start), so a claim's RunID IS the workflow id to describe.
// It satisfies localscheduler.RunLivenessProbe; the dependency points from
// the composition root (cmd/goobers), never from localscheduler to here.
type WorkflowLiveness struct {
	client workflowLivenessClient
}

// NewWorkflowLiveness builds the probe over a Temporal client.
func NewWorkflowLiveness(c workflowLivenessClient) *WorkflowLiveness {
	return &WorkflowLiveness{client: c}
}

// RunLive reports (true, nil) for an open workflow and (false, nil) —
// definitively not live — for a closed or vanished one: NotFound means the
// workflow (or its history) is gone, which DS6 treats the same as closed,
// letting the lease lapse normally. Any other error is UNKNOWN liveness and
// returned so the renewal layer can fail live.
func (p *WorkflowLiveness) RunLive(ctx context.Context, runID string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, describeLivenessTimeout)
	defer cancel()
	resp, err := p.client.DescribeWorkflowExecution(ctx, runID, "")
	if err != nil {
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			return false, nil
		}
		return false, fmt.Errorf("engine: describe workflow %s: %w", runID, err)
	}
	return resp.GetWorkflowExecutionInfo().GetStatus() == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, nil
}
