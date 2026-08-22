package engine

import (
	"context"
	"errors"
	"testing"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
)

type fakeDescriber struct {
	status     enumspb.WorkflowExecutionStatus
	err        error
	workflowID string

	open      []string
	listErr   error
	listCalls int
	listPages int
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

func (f *fakeDescriber) ListWorkflow(_ context.Context, request *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.listPages > 0 {
		// Simulate an enumeration that never terminates within the page cap.
		return &workflowservice.ListWorkflowExecutionsResponse{NextPageToken: []byte("more")}, nil
	}
	resp := &workflowservice.ListWorkflowExecutionsResponse{}
	for _, id := range f.open {
		resp.Executions = append(resp.Executions, &workflowpb.WorkflowExecutionInfo{
			Execution: &commonpb.WorkflowExecution{WorkflowId: id},
		})
	}
	return resp, nil
}

// TestWorkflowLivenessAnswers is DS6's engine liveness half
// (distributed-state-and-coordination.md §10): an open workflow is live; a
// closed one is DEFINITIVELY not live so its claim lease lapses normally; a
// vanished (NotFound) one is definitively not live only after the
// scheduled-run mapping also comes up empty; any other describe failure is
// unknown — an error the renewal layer fails live on, since an unreachable
// frontend proves neither.
func TestWorkflowLivenessAnswers(t *testing.T) {
	ctx := context.Background()

	open := &fakeDescriber{status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING}
	if live, err := NewWorkflowLiveness(open, "default").RunLive(ctx, "run-1"); err != nil || !live {
		t.Fatalf("open workflow: live=%v err=%v, want live", live, err)
	}
	// The engine starts each directly started run's workflow with
	// WorkflowID == RunID (TemporalStarter.Start), so the claim's RunID is
	// the id described.
	if open.workflowID != "run-1" {
		t.Fatalf("described workflow id = %q, want the claim's run id", open.workflowID)
	}
	if open.listCalls != 0 {
		t.Fatalf("open workflow: %d open-workflow scans, want 0 — describe already answered", open.listCalls)
	}

	closed := &fakeDescriber{status: enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED}
	if live, err := NewWorkflowLiveness(closed, "default").RunLive(ctx, "run-2"); err != nil || live {
		t.Fatalf("closed workflow: live=%v err=%v, want definitively not live", live, err)
	}

	vanished := &fakeDescriber{err: serviceerror.NewNotFound("workflow not found")}
	if live, err := NewWorkflowLiveness(vanished, "default").RunLive(ctx, "run-3"); err != nil || live {
		t.Fatalf("vanished workflow: live=%v err=%v, want definitively not live, nil error", live, err)
	}

	unreachable := &fakeDescriber{err: errors.New("connection refused")}
	if live, err := NewWorkflowLiveness(unreachable, "default").RunLive(ctx, "run-4"); err == nil || live {
		t.Fatalf("unreachable frontend: live=%v err=%v, want unknown (error)", live, err)
	}
}

// TestWorkflowLivenessScheduledRun is the RunID==WorkflowID mismatch the
// scheduled path creates (issue #3512 review): RunScheduled rewrites the
// run's RunID to RunID(claimID) — a hash no workflow executes under — while
// the open workflows are the Schedule claim (claimID) and its child
// (claimID+"-run"). Describe(hash) is NotFound, so without the open-workflow
// mapping the probe answered DEFINITIVELY not live and renewal silently never
// covered scheduled runs. The probe must report the run live while either
// workflow is open, and definitively not live once neither is.
func TestWorkflowLivenessScheduledRun(t *testing.T) {
	ctx := context.Background()
	claimID := "goobers-aaaa-bbbb-2026-08-22T01:00:00Z"
	hashedRunID := RunID(claimID)

	// Open under both the claim workflow and its child (the running shape).
	running := &fakeDescriber{
		err:  serviceerror.NewNotFound("workflow not found"),
		open: []string{claimID, scheduledRunWorkflowID(claimID)},
	}
	if live, err := NewWorkflowLiveness(running, "default").RunLive(ctx, hashedRunID); err != nil || !live {
		t.Fatalf("scheduled run with open claim workflow: live=%v err=%v, want live", live, err)
	}

	// Open under only the child covers a claim workflow that closed early.
	childOnly := &fakeDescriber{
		err:  serviceerror.NewNotFound("workflow not found"),
		open: []string{scheduledRunWorkflowID(claimID)},
	}
	if live, err := NewWorkflowLiveness(childOnly, "default").RunLive(ctx, hashedRunID); err != nil || !live {
		t.Fatalf("scheduled run with open child workflow: live=%v err=%v, want live", live, err)
	}

	// A genuinely closed scheduled workflow no longer appears in the open
	// set: the lease lapses normally.
	closedScheduled := &fakeDescriber{
		err:  serviceerror.NewNotFound("workflow not found"),
		open: []string{"some-unrelated-workflow"},
	}
	if live, err := NewWorkflowLiveness(closedScheduled, "default").RunLive(ctx, hashedRunID); err != nil || live {
		t.Fatalf("closed scheduled run: live=%v err=%v, want definitively not live", live, err)
	}
}

// TestWorkflowLivenessScheduledRunUnknownMappingFailsLive: when the
// open-workflow enumeration cannot complete — the visibility store errors, or
// the scan exceeds its page cap — the mapping is UNKNOWN, which must surface
// as an error (renewal fails live), never as definitively not-live.
func TestWorkflowLivenessScheduledRunUnknownMappingFailsLive(t *testing.T) {
	ctx := context.Background()
	hashedRunID := RunID("goobers-schedule-claim")

	listBroken := &fakeDescriber{
		err:     serviceerror.NewNotFound("workflow not found"),
		listErr: errors.New("visibility store unavailable"),
	}
	if live, err := NewWorkflowLiveness(listBroken, "default").RunLive(ctx, hashedRunID); err == nil || live {
		t.Fatalf("list error: live=%v err=%v, want unknown (error)", live, err)
	}

	unbounded := &fakeDescriber{
		err:       serviceerror.NewNotFound("workflow not found"),
		listPages: 1,
	}
	if live, err := NewWorkflowLiveness(unbounded, "default").RunLive(ctx, hashedRunID); err == nil || live {
		t.Fatalf("page cap exceeded: live=%v err=%v, want unknown (error)", live, err)
	}
}

// TestWorkflowLivenessScansOncePerPass: probing several hash-shaped RunIDs
// within the cache TTL performs a single open-workflow enumeration — one
// visibility scan serves the whole renewal pass.
func TestWorkflowLivenessScansOncePerPass(t *testing.T) {
	ctx := context.Background()
	claimID := "goobers-aaaa-bbbb-2026-08-22T02:00:00Z"
	fake := &fakeDescriber{
		err:  serviceerror.NewNotFound("workflow not found"),
		open: []string{claimID},
	}
	probe := NewWorkflowLiveness(fake, "default")
	if live, err := probe.RunLive(ctx, RunID(claimID)); err != nil || !live {
		t.Fatalf("first probe: live=%v err=%v, want live", live, err)
	}
	if live, err := probe.RunLive(ctx, RunID("goobers-other-claim")); err != nil || live {
		t.Fatalf("second probe: live=%v err=%v, want not live", live, err)
	}
	if fake.listCalls != 1 {
		t.Fatalf("open-workflow scans = %d, want 1 (cached within the pass)", fake.listCalls)
	}
}
