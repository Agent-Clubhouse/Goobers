package engine

import (
	"context"
	"errors"
	"testing"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
)

type fakeDescriber struct {
	status     enumspb.WorkflowExecutionStatus
	err        error
	workflowID string
}

func (f *fakeDescriber) DescribeWorkflowExecution(_ context.Context, workflowID, runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	f.workflowID = workflowID
	if f.err != nil {
		return nil, f.err
	}
	return &workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: f.status},
	}, nil
}

// TestWorkflowLivenessAnswers is DS6's engine liveness half
// (distributed-state-and-coordination.md §10): an open workflow is live; a
// closed or vanished (NotFound) one is DEFINITIVELY not live so its claim
// lease lapses normally; any other describe failure is unknown — an error the
// renewal layer fails live on, since an unreachable frontend proves neither.
func TestWorkflowLivenessAnswers(t *testing.T) {
	ctx := context.Background()

	open := &fakeDescriber{status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING}
	if live, err := NewWorkflowLiveness(open).RunLive(ctx, "run-1"); err != nil || !live {
		t.Fatalf("open workflow: live=%v err=%v, want live", live, err)
	}
	// The engine starts each run's workflow with WorkflowID == RunID
	// (TemporalStarter.Start), so the claim's RunID is the id described.
	if open.workflowID != "run-1" {
		t.Fatalf("described workflow id = %q, want the claim's run id", open.workflowID)
	}

	closed := &fakeDescriber{status: enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED}
	if live, err := NewWorkflowLiveness(closed).RunLive(ctx, "run-2"); err != nil || live {
		t.Fatalf("closed workflow: live=%v err=%v, want definitively not live", live, err)
	}

	vanished := &fakeDescriber{err: serviceerror.NewNotFound("workflow not found")}
	if live, err := NewWorkflowLiveness(vanished).RunLive(ctx, "run-3"); err != nil || live {
		t.Fatalf("vanished workflow: live=%v err=%v, want definitively not live, nil error", live, err)
	}

	unreachable := &fakeDescriber{err: errors.New("connection refused")}
	if live, err := NewWorkflowLiveness(unreachable).RunLive(ctx, "run-4"); err == nil || live {
		t.Fatalf("unreachable frontend: live=%v err=%v, want unknown (error)", live, err)
	}
}
