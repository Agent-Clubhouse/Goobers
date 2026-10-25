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
	failurepb "go.temporal.io/api/failure/v1"
	"go.temporal.io/sdk/client"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
)

func TestChildQualificationWorkerLoss(t *testing.T) {
	t.Run("custody-adoption", func(t *testing.T) {
		h := newQualificationHarness(t)
		qualificationRetryParent(h)
		barrier := h.StageBarrier("live-grant")
		h.SetStageScript(qualificationStageScript{"parent-granted": {
			Barrier: "live-grant",
			Result: &apiv1.ResultEnvelope{Status: apiv1.ResultFailure,
				Error: &apiv1.ErrorInfo{Code: "original_attempt_failed", Message: "original retained executor failed", Retryable: true}},
		}})
		run := h.StartParent()
		if err := barrier.Wait(h.ctx); err != nil {
			t.Fatal(err)
		}
		custody := h.WaitForPodCustody(run)
		oldRequest := qualificationBoundRequest(h)
		oldGrant := qualificationGrant(h, oldRequest)
		h.KillWorker()

		adoption := h.InstallHTTPBarrier(qualificationAuthorityRequest)
		h.SetStageScript(qualificationStageScript{"parent-start": {Result: &apiv1.ResultEnvelope{
			Status: apiv1.ResultFailure,
			Error:  &apiv1.ErrorInfo{Code: "replacement_attempt_failed", Message: "replacement attempt failed"}}}})
		h.StartWorker()
		if err := adoption.Wait(h.ctx); err != nil {
			t.Fatal(err)
		}
		var rebound bool
		if err := h.pool.QueryRow(h.ctx, `SELECT count(*)=1 AND bool_and(grant_id=$3 AND attempt_id=$4 AND NOT revoked)
			FROM control.child_authority WHERE instance_id=$1 AND parent_run_id=$2`,
			testInstance, run.GetID(), oldGrant.ID, oldGrant.AttemptID).Scan(&rebound); err != nil || !rebound {
			t.Fatalf("reattach replaced the original authority: %t (%v)", rebound, err)
		}
		if starts := qualificationStageStarts(h); len(starts) != 1 || !starts[oldGrant.AttemptID] {
			t.Fatalf("reattach journaled a fresh stage attempt: %v", starts)
		}
		adoption.Release()
		adopted := h.WaitForPodCustody(run)
		if adopted != custody {
			t.Fatalf("replacement custody=%+v, want retained %+v", adopted, custody)
		}
		h.pods.mu.Lock()
		created := h.pods.created
		retained := h.pods.pods[custody.Namespace+"/"+custody.Pod]
		h.pods.mu.Unlock()
		if created != 1 || retained == nil || string(retained.pod.UID) != custody.UID {
			t.Fatalf("pod custody was replaced: created=%d retained=%t", created, retained != nil)
		}

		replacement := h.InstallHTTPBarrier(qualificationAuthorityRequest)
		barrier.Release()
		if err := replacement.Wait(h.ctx); err != nil {
			t.Fatal(err)
		}
		qualificationDispatchHistory(h, run, 3, 1, false)
		newRequest := qualificationBoundRequest(h)
		newGrant := qualificationGrant(h, newRequest)
		if newGrant.ID == oldGrant.ID || newGrant.AttemptID == oldGrant.AttemptID || newGrant.StageOccurrence != oldGrant.StageOccurrence {
			t.Fatalf("retry authority did not advance the attempt: old=%+v new=%+v", oldGrant, newGrant)
		}
		qualificationProbe(h, oldRequest, apicontract.ChildWorkflowStartPath, "stale-attempt", http.StatusForbidden, "child_workflow_authority_changed")
		var current bool
		if err := h.pool.QueryRow(h.ctx, `SELECT a.grant_id=$3 AND a.attempt_id=$4 AND NOT a.revoked AND p.cancelled_at IS NULL
			FROM control.child_authority a JOIN control.child_parent p USING (instance_id,gaggle,parent_run_id)
			WHERE a.instance_id=$1 AND a.parent_run_id=$2`,
			testInstance, run.GetID(), newGrant.ID, newGrant.AttemptID).Scan(&current); err != nil || !current {
			t.Fatalf("replacement authority custody: %t (%v)", current, err)
		}
		replacement.Release()

		var result engine.RunResult
		if err := run.Get(h.ctx, &result); err != nil || result.Status != engine.StatusFailed ||
			result.FailureCode != "replacement_attempt_failed" {
			t.Fatalf("parent terminal failure: %+v (%v)", result, err)
		}
		starts := qualificationStageStarts(h)
		finished := map[int]string{}
		finishedCount := 0
		var retryClass journal.AttemptClass
		for _, event := range qualificationEvents(h) {
			if event.Type == journal.EventStageFinished {
				finishedCount++
				finished[event.Attempt] = event.Status
			}
			if event.Type == journal.EventStageStarted && event.Attempt == 2 {
				retryClass = event.AttemptClass
			}
		}
		if len(starts) != 2 || !starts[oldGrant.AttemptID] || !starts[newGrant.AttemptID] ||
			finishedCount != 2 || finished[1] != string(apiv1.ResultFailure) ||
			finished[2] != string(apiv1.ResultFailure) || retryClass != journal.AttemptInfra {
			t.Fatalf("attempt journal: starts=%v finished=%v count=%d retryClass=%q", starts, finished, finishedCount, retryClass)
		}
		qualificationNoChildWait(h)
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
	t.Run("fail-closed-without-custody", func(t *testing.T) {
		h := newQualificationHarness(t)
		qualificationRetryParent(h)
		h.SetPodCustodyReporting(false)
		barrier := h.StageBarrier("live-grant")
		h.SetStageScript(qualificationStageScript{"parent-granted": {Barrier: "live-grant"}})
		run := h.StartParent()
		if err := barrier.Wait(h.ctx); err != nil {
			t.Fatal(err)
		}
		request := qualificationBoundRequest(h)
		grant := qualificationGrant(h, request)
		h.KillWorker()
		h.StartWorker()

		var result engine.RunResult
		if err := run.Get(h.ctx, &result); err == nil {
			t.Fatalf("missing-custody parent unexpectedly returned a result: %+v", result)
		}
		qualificationDispatchHistory(h, run, 1, 1, true)
		h.pods.mu.Lock()
		created := h.pods.created
		h.pods.mu.Unlock()
		if created != 1 {
			t.Fatalf("missing custody caused %d pod dispatches, want 1", created)
		}
		if starts := qualificationStageStarts(h); len(starts) != 1 || !starts[grant.AttemptID] {
			t.Fatalf("missing custody journaled a fresh attempt: %v", starts)
		}
		for _, event := range qualificationEvents(h) {
			if event.Type == journal.EventStageFinished || event.Type == journal.EventRunFinished {
				t.Fatalf("fail-closed worker loss journaled a terminal result: %+v", event)
			}
		}
		var retained bool
		if err := h.pool.QueryRow(h.ctx, `SELECT NOT revoked AND expires_at>clock_timestamp()
			FROM control.child_authority WHERE instance_id=$1 AND parent_run_id=$2 AND grant_id=$3`,
			testInstance, run.GetID(), grant.ID).Scan(&retained); err != nil || !retained {
			t.Fatalf("fail-closed grant was revoked or expired: %t (%v)", retained, err)
		}
		qualificationProbe(h, request, apicontract.ChildWorkflowStartPath, "after-worker-loss", http.StatusForbidden, "child_workflow_authority_changed")
		qualificationNoChildWait(h)
		qualificationNoChildren(h)
	})
}

func qualificationAuthorityRequest(request *http.Request) bool {
	return request.Method == http.MethodPost && request.URL.Path == apicontract.EngineChildAuthorityPath
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

func qualificationNoChildWait(h *qualificationHarness) {
	h.t.Helper()
	for _, event := range qualificationEvents(h) {
		if event.Type == journal.EventRunnerAnnotation {
			kind, _ := event.Runner["kind"].(string)
			if kind == journal.ChildWaitKind {
				h.t.Fatal("worker loss fabricated a ChildWait")
			}
		}
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

func qualificationStageStarts(h *qualificationHarness) map[string]bool {
	h.t.Helper()
	attempts := map[string]bool{}
	for _, event := range qualificationEvents(h) {
		if event.Type != journal.EventStageStarted {
			continue
		}
		origin, err := journal.ChildWorkflowOriginForEvent(h.input.RunID, event)
		if err != nil {
			h.t.Fatal(err)
		}
		if origin != nil {
			attempts[origin.AttemptID] = true
		}
	}
	return attempts
}

func qualificationDispatchHistory(h *qualificationHarness, run client.WorkflowRun, wantScheduled, wantTimeouts int, wantHandoffFailure bool) {
	h.t.Helper()
	history := h.temporal.Client().GetWorkflowHistory(h.ctx, run.GetID(), run.GetRunID(), false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	stages := map[int64]bool{}
	scheduled := 0
	timeouts := 0
	handoffFailure := false
	for history.HasNext() {
		event, err := history.Next()
		if err != nil {
			h.t.Fatal(err)
		}
		if attributes := event.GetActivityTaskScheduledEventAttributes(); attributes != nil {
			stage := attributes.GetActivityType().GetName() == engine.ActDispatchStage
			stages[event.GetEventId()] = stage
			if stage {
				scheduled++
			}
		}
		if timeout := event.GetActivityTaskTimedOutEventAttributes(); timeout != nil && stages[timeout.GetScheduledEventId()] {
			if timeout.GetFailure().GetTimeoutFailureInfo().GetTimeoutType() != enumspb.TIMEOUT_TYPE_HEARTBEAT {
				h.t.Fatal("DispatchStage timed out for a reason other than lost worker heartbeats")
			}
			timeouts++
		}
		if failed := event.GetWorkflowExecutionFailedEventAttributes(); failed != nil {
			handoffFailure = qualificationFailureType(failed.GetFailure(), "GoobersChildHandoffUnconfirmed")
		}
	}
	if scheduled != wantScheduled || timeouts != wantTimeouts || handoffFailure != wantHandoffFailure {
		h.t.Fatalf("DispatchStage history: scheduled=%d timeouts=%d childHandoffFailure=%t, want %d/%d/%t",
			scheduled, timeouts, handoffFailure, wantScheduled, wantTimeouts, wantHandoffFailure)
	}
}

func qualificationFailureType(failure *failurepb.Failure, want string) bool {
	for current := failure; current != nil; current = current.GetCause() {
		if current.GetApplicationFailureInfo().GetType() == want {
			return true
		}
	}
	return false
}
