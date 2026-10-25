//go:build cloudintegration

package childengine

import (
	"encoding/json"
	"net/http"
	"testing"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
)

func TestChildQualificationWorkerLoss(t *testing.T) {
	t.Run("completion", func(t *testing.T) {
		h := newQualificationHarness(t)
		barrier := h.StageBarrier("live-grant")
		h.SetStageScript(qualificationStageScript{"parent-granted": {Barrier: "live-grant"}})
		run := h.StartParent()
		if err := barrier.Wait(h.ctx); err != nil {
			t.Fatal(err)
		}
		request := qualificationBoundRequest(h)
		grant := qualificationGrant(h, request)
		h.KillWorker()
		qualificationRunning(h, run)
		qualificationLiveGrant(h, request)
		// Local stage activities have no heartbeat (#8230); their timeout
		// exceeds this fixture. Report the scripted executor result via Temporal
		// RPC after killing the worker. Production Run commits stage/run finish.
		// The dead worker cannot execute its deferred grant release, so the
		// eventual refusal specifically exercises the parent-liveness boundary.
		qualificationSettleInterrupted(h, run, engine.DispatchStageResult{ResultEnvelope: apiv1.ResultEnvelope{
			Status: apiv1.ResultSuccess, Summary: "interrupted executor settled"}}, nil)
		h.StartWorker()
		var result engine.RunResult
		if err := run.Get(h.ctx, &result); err != nil || result.Status != engine.StatusCompleted {
			t.Fatalf("parent did not complete: %+v (%v)", result, err)
		}
		var live bool
		if err := h.pool.QueryRow(h.ctx, `SELECT NOT revoked AND expires_at>clock_timestamp() FROM control.child_authority
			WHERE instance_id=$1 AND parent_run_id=$2 AND grant_id=$3`, testInstance, run.GetID(), grant.ID).Scan(&live); err != nil || !live {
			t.Fatalf("completion probe requires an unrevoked, unexpired binding: %t (%v)", live, err)
		}
		qualificationProbe(h, request, apicontract.ChildWorkflowStartPath, "after-completion", http.StatusForbidden, "child_workflow_authority_changed")
		finished := 0
		for _, event := range qualificationEvents(h) {
			if event.Type == journal.EventRunFinished {
				finished++
			}
		}
		if finished != 1 {
			t.Fatalf("parent run.finished events=%d, want 1", finished)
		}
		qualificationNoChildren(h)
	})
	t.Run("ttl", func(t *testing.T) {
		h := newQualificationHarness(t)
		barrier := h.StageBarrier("live-grant")
		h.SetStageScript(qualificationStageScript{"parent-granted": {Barrier: "live-grant"}})
		run := h.StartParent()
		if err := barrier.Wait(h.ctx); err != nil {
			t.Fatal(err)
		}
		request := qualificationBoundRequest(h)
		grant := qualificationGrant(h, request)
		h.KillWorker()
		qualificationRunning(h, run)
		qualificationLiveGrant(h, request)
		// Advance only the retained grant's expiry past the database clock.
		// The signed bearer remains unexpired: the production custody check,
		// not a rewritten token or a mock clock, must refuse the stale grant.
		qualificationAge(h, `UPDATE control.child_authority SET expires_at=clock_timestamp()-interval '1 second'
			WHERE instance_id=$1 AND parent_run_id=$2 AND grant_id=$3 AND NOT revoked`, testInstance, run.GetID(), grant.ID)
		qualificationProbe(h, request, apicontract.ChildWorkflowStartPath, "after-ttl", http.StatusForbidden, "child_workflow_authority_changed")
		qualificationRunning(h, run)
		for _, event := range qualificationEvents(h) {
			if event.Type == journal.EventStageFinished || event.Type == journal.EventRunFinished {
				t.Fatal("TTL refusal was masked by a finished stage or parent")
			}
		}
		qualificationNoChildren(h)
	})
	t.Run("replacement", func(t *testing.T) {
		h := newQualificationHarness(t)
		barrier := h.StageBarrier("live-grant")
		h.SetStageScript(qualificationStageScript{"parent-granted": {Barrier: "live-grant"}})
		run := h.StartParent()
		if err := barrier.Wait(h.ctx); err != nil {
			t.Fatal(err)
		}
		oldRequest := qualificationBoundRequest(h)
		oldGrant := qualificationGrant(h, oldRequest)
		h.KillWorker()
		// With no local-stage heartbeat (#8230), report the interrupted
		// scripted executor through Temporal's activity
		// result API. This does not qualify automatic worker-loss detection:
		// the retry, replacement origin and bound grant are production paths.
		qualificationSettleInterrupted(h, run, nil, temporal.NewApplicationError("qualification executor interrupted", engine.FailureTypeInfrastructure))
		replacement := h.StageBarrier("replacement-grant")
		h.SetStageScript(qualificationStageScript{"parent-granted": {Barrier: "replacement-grant"}})
		h.StartWorker()
		if err := replacement.Wait(h.ctx); err != nil {
			t.Fatal(err)
		}
		newRequest := qualificationBoundRequest(h)
		newGrant := qualificationGrant(h, newRequest)
		if newGrant.ID == oldGrant.ID || newGrant.AttemptID == oldGrant.AttemptID || newGrant.StageOccurrence != oldGrant.StageOccurrence {
			t.Fatal("restarted worker did not bind a fresh grant for the replacement attempt")
		}
		qualificationLiveGrant(h, newRequest)
		qualificationProbe(h, oldRequest, apicontract.ChildWorkflowStartPath, "stale-attempt", http.StatusForbidden, "child_workflow_authority_changed")
		var replaced bool
		if err := h.pool.QueryRow(h.ctx, `SELECT grant_id=$3 AND attempt_id=$4 AND NOT revoked FROM control.child_authority
			WHERE instance_id=$1 AND parent_run_id=$2`, testInstance, run.GetID(), newGrant.ID, newGrant.AttemptID).Scan(&replaced); err != nil || !replaced {
			t.Fatalf("replacement grant custody: %t (%v)", replaced, err)
		}
		attempts := map[string]bool{}
		for _, event := range qualificationEvents(h) {
			if event.Type != journal.EventStageStarted {
				continue
			}
			origin, err := journal.ChildWorkflowOriginForEvent(run.GetID(), event)
			if err != nil {
				t.Fatal(err)
			}
			if origin != nil {
				attempts[origin.AttemptID] = true
			}
		}
		if !attempts[oldGrant.AttemptID] || !attempts[newGrant.AttemptID] {
			t.Fatal("journal lacks the original and replacement stage attempts")
		}
		qualificationNoChildren(h)
		replacement.Release()
		qualificationCompleted(h, run)
	})
}

func qualificationRunning(h *qualificationHarness, run client.WorkflowRun) {
	h.t.Helper()
	description, err := h.temporal.Client().DescribeWorkflowExecution(h.ctx, run.GetID(), run.GetRunID())
	if err != nil || description.GetWorkflowExecutionInfo().GetStatus() != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
		h.t.Fatalf("parent is not still RUNNING: %v", err)
	}
}

func qualificationLiveGrant(h *qualificationHarness, request *qualificationSubmission) {
	h.t.Helper()
	raw := qualificationProbe(h, request, apicontract.ChildWorkflowValidatePath, "", http.StatusOK, "")
	var result apicontract.ChildWorkflowValidationResponse
	if err := json.Unmarshal(raw, &result); err != nil || !result.Valid {
		h.t.Fatalf("live grant did not validate the child: valid=%t (%v)", result.Valid, err)
	}
}

func qualificationNoChildren(h *qualificationHarness) {
	h.t.Helper()
	var count int
	if err := h.pool.QueryRow(h.ctx, `SELECT count(*) FROM control.child_lineage WHERE instance_id=$1 AND parent_run_id=$2`, testInstance, h.input.RunID).Scan(&count); err != nil || count != 0 {
		h.t.Fatalf("refused or advisory request created child custody: %d (%v)", count, err)
	}
}

func qualificationSettleInterrupted(h *qualificationHarness, run client.WorkflowRun, result any, failure error) {
	h.t.Helper()
	description, err := h.temporal.Client().DescribeWorkflowExecution(h.ctx, run.GetID(), run.GetRunID())
	if err != nil {
		h.t.Fatal(err)
	}
	var activityID string
	for _, pending := range description.PendingActivities {
		if pending.ActivityType.GetName() == engine.ActInvokeGoober || pending.ActivityType.GetName() == "InvokeGooberCancelSafe" {
			if activityID != "" || pending.State != enumspb.PENDING_ACTIVITY_STATE_STARTED {
				h.t.Fatal("interrupted executor is not one started activity")
			}
			activityID = pending.ActivityId
		}
	}
	if activityID == "" {
		h.t.Fatal("no interrupted stage activity in Temporal")
	}
	if err := h.temporal.Client().CompleteActivityByID(h.ctx, "default", run.GetID(), run.GetRunID(), activityID, result, failure); err != nil {
		h.t.Fatal(err)
	}
}
