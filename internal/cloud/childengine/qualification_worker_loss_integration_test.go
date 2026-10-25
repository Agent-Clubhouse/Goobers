//go:build cloudintegration

package childengine

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
)

func TestChildQualificationWorkerLoss(t *testing.T) {
	t.Run("completion", func(t *testing.T) {
		h := newQualificationHarness(t)
		qualificationRetryParent(h)
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
		// The shared script heartbeats like production's cancel-safe executor.
		// Killing it stops those heartbeats (#8230); Temporal times out the
		// activity and production Run schedules the second stage attempt.
		// This replacement executor succeeds without acquiring/releasing the
		// old grant, isolating refusal after the parent's actual completion.
		h.SetStageScript(qualificationStageScript{"parent-start": {Result: &apiv1.ResultEnvelope{
			Status: apiv1.ResultSuccess, Summary: "replacement executor completed"}}})
		h.StartWorker()
		var result engine.RunResult
		if err := run.Get(h.ctx, &result); err != nil || result.Status != engine.StatusCompleted || result.Outputs["plan"].Summary != "replacement executor completed" {
			t.Fatalf("parent did not complete: %+v (%v)", result, err)
		}
		qualificationHeartbeatTimeout(h, run)
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
		qualificationRetryParent(h)
		barrier := h.StageBarrier("live-grant")
		h.SetStageScript(qualificationStageScript{"parent-granted": {Barrier: "live-grant"}})
		run := h.StartParent()
		if err := barrier.Wait(h.ctx); err != nil {
			t.Fatal(err)
		}
		oldRequest := qualificationBoundRequest(h)
		oldGrant := qualificationGrant(h, oldRequest)
		h.KillWorker()
		// No activity result is injected: the killed worker stops heartbeating,
		// and the real timeout/retry creates the replacement stage attempt.
		replacement := h.StageBarrier("replacement-grant")
		h.SetStageScript(qualificationStageScript{"parent-granted": {Barrier: "replacement-grant"}})
		h.StartWorker()
		if err := replacement.Wait(h.ctx); err != nil {
			t.Fatal(err)
		}
		qualificationHeartbeatTimeout(h, run)
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
				if origin.AttemptID == newGrant.AttemptID && event.AttemptClass != journal.AttemptInfra {
					t.Fatalf("replacement attempt class=%q, want infrastructure", event.AttemptClass)
				}
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

// qualificationRetryParent publishes a real two-attempt definition before the
// parent starts, then rebuilds every affected start pin from that generation.
// The workflow also applies its independent infrastructure budget to heartbeat
// loss on this stage, which declares no policy actions.
func qualificationRetryParent(h *qualificationHarness) {
	h.t.Helper()
	generation := h.ReloadConfig(func(directory string) {
		definition := strings.Replace(childEnabledParent, "      goal: Plan a child workflow", "      retry: {maxAttempts: 2}\n      goal: Plan a child workflow", 1)
		if err := os.WriteFile(filepath.Join(directory, "gaggles", "example", "workflows", "default-implement.yaml"), []byte(definition), 0o600); err != nil {
			h.t.Fatal(err)
		}
	})
	candidate, _ := h.applied.Applied()
	_, machine, err := childworkflow.StageCatalog(candidate.Config, candidate.Set,
		childworkflow.ParentSelection{Gaggle: "example", Workflow: "default-implement", Stage: "plan"},
		childworkflow.BackendEngine, childworkflow.OfflineGooberAdmission)
	if err != nil {
		h.t.Fatal(err)
	}
	h.input.Spec = machine.Def.Spec
	h.input.ConfigGeneration = generation
	h.input.WorkflowDigest, h.input.GooberDigest, _ = parentPins(h.t, candidate.Config, h.configDir, candidate.Set)
}

func qualificationHeartbeatTimeout(h *qualificationHarness, run client.WorkflowRun) {
	h.t.Helper()
	history := h.temporal.Client().GetWorkflowHistory(h.ctx, run.GetID(), run.GetRunID(), false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	stages := map[int64]bool{}
	timeouts := 0
	for history.HasNext() {
		event, err := history.Next()
		if err != nil {
			h.t.Fatal(err)
		}
		if scheduled := event.GetActivityTaskScheduledEventAttributes(); scheduled != nil {
			name := scheduled.GetActivityType().GetName()
			stages[event.GetEventId()] = name == engine.ActInvokeGoober || name == "InvokeGooberCancelSafe"
		}
		if timeout := event.GetActivityTaskTimedOutEventAttributes(); timeout != nil && stages[timeout.GetScheduledEventId()] {
			if timeout.GetFailure().GetTimeoutFailureInfo().GetTimeoutType() != enumspb.TIMEOUT_TYPE_HEARTBEAT {
				h.t.Fatal("stage timed out for a reason other than lost worker heartbeats")
			}
			timeouts++
		}
	}
	if timeouts != 1 {
		h.t.Fatalf("stage HEARTBEAT timeout events=%d, want 1", timeouts)
	}
}
