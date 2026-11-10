//go:build cloudintegration

package childengine

import (
	"strings"
	"testing"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	"github.com/goobers/goobers/internal/engine"
)

// Scenario 5 resets the real parent to the workflow task that originally
// started its child. Phase 1 must refuse restarting that accepted child.
func TestChildQualificationParentReset(t *testing.T) {
	h := newQualificationHarness(t)
	run := h.StartParent()
	qualificationCompleted(t, h, run)
	var childID, receipt, manifest, owner string
	if err := h.pool.QueryRow(h.ctx, `SELECT l.child_id, l.result_receipt_digest, s.manifest_digest, s.parent_temporal_run_id
 FROM control.child_lineage l JOIN control.child_engine_start s USING (instance_id,child_id)
 WHERE l.instance_id=$1 AND l.parent_run_id=$2`, testInstance, run.GetID()).Scan(&childID, &receipt, &manifest, &owner); err != nil {
		t.Fatal(err)
	}
	if receipt == "" || manifest == "" || owner != run.GetRunID() {
		t.Fatalf("incomplete original custody: child=%s receipt=%s owner=%s", childID, receipt, owner)
	}
	history := h.temporal.Client().GetWorkflowHistory(h.ctx, run.GetID(), run.GetRunID(), false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	var resetTo int64
	var childWorkflow, childExecution string
	for history.HasNext() {
		event, err := history.Next()
		if err != nil {
			t.Fatal(err)
		}
		if event.GetEventType() == enumspb.EVENT_TYPE_WORKFLOW_TASK_COMPLETED && childWorkflow == "" {
			resetTo = event.GetEventId()
		}
		if started := event.GetStartChildWorkflowExecutionInitiatedEventAttributes(); started != nil {
			childWorkflow = started.GetWorkflowId()
		}
		if started := event.GetChildWorkflowExecutionStartedEventAttributes(); started != nil {
			childExecution = started.GetWorkflowExecution().GetRunId()
		}
	}
	if resetTo == 0 || childWorkflow == "" || childExecution == "" {
		t.Fatal("missing durable child start/reset boundary")
	}
	resetID := h.ResetParent(run, resetTo)
	if resetID == run.GetRunID() {
		t.Fatal("reset did not create a new execution")
	}
	err := h.temporal.Client().GetWorkflow(h.ctx, run.GetID(), resetID).Get(h.ctx, nil)
	if err == nil || !strings.Contains(err.Error(), "restarting an accepted generated child is unsupported") {
		t.Fatalf("reset must explicitly refuse child restart: %v", err)
	}
	var count int
	if err := h.pool.QueryRow(h.ctx, `SELECT count(*) FROM control.child_lineage WHERE instance_id=$1`, testInstance).Scan(&count); err != nil || count != 1 {
		t.Fatalf("child count=%d: %v", count, err)
	}
	if err := h.pool.QueryRow(h.ctx, `SELECT count(*) FROM control.child_lineage l JOIN control.child_engine_start s USING (instance_id,child_id)
 WHERE l.instance_id=$1 AND l.child_id=$2 AND l.state='completed' AND l.result_receipt_digest=$3 AND l.result_ref=$3
 AND l.terminal_at IS NOT NULL AND l.tombstoned_at IS NULL AND s.manifest_digest=$4 AND s.parent_temporal_run_id=$5`, testInstance, childID, receipt, manifest, owner).Scan(&count); err != nil || count != 1 {
		t.Fatalf("reset changed or orphaned sealed custody: count=%d: %v", count, err)
	}
	description, err := h.temporal.Client().DescribeWorkflowExecution(h.ctx, childWorkflow, "")
	if err != nil {
		t.Fatal(err)
	}
	if description.WorkflowExecutionInfo.GetExecution().GetRunId() != childExecution || description.WorkflowExecutionInfo.GetStatus() != enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED {
		t.Fatalf("child was restarted or left running: %v", description.WorkflowExecutionInfo)
	}
	var childResult engine.RunResult
	if err := h.temporal.Client().GetWorkflow(h.ctx, childWorkflow, childExecution).Get(h.ctx, &childResult); err != nil || childResult.ChildResult == nil || childResult.ChildResult.ResultRef != receipt {
		t.Fatalf("original sealed result lost: %+v: %v", childResult, err)
	}
	t.Log("qualification scenario 5: real Temporal reset explicitly refused restart; one completed child and its original sealed custody remain")
}

func qualificationCompleted(t *testing.T, h *qualificationHarness, run client.WorkflowRun) {
	t.Helper()
	var result engine.RunResult
	if err := run.Get(h.ctx, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != engine.StatusCompleted || result.Outputs["plan"].Summary != "parent read child receipt through blob plane" {
		t.Fatalf("parent result: %+v", result)
	}
	h.assertProductionChildHandoff(run)
}
