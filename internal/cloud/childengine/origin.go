package childengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"

	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/cloud/startfence"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/livejournal"
)

// OriginTemporal reads the engine's actual resident activity loan. Neither a
// caller-supplied stage identity nor a signed worker identity alone grants it.
// Production derives both the authenticated worker ID and Temporal identity
// from workerhost.Identity at startup. LastWorkerIdentity is self-reported:
// a compromised peer holding the shared signing key remains a trusted-host
// compromise, and can impersonate another worker. Pods hold no such key.
type OriginTemporal interface {
	DescribeWorkflowExecution(context.Context, string, string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
	GetWorkflowHistory(context.Context, string, string, bool, enumspb.HistoryEventFilterType) client.HistoryEventIterator
}

// EmitChildOrigin uses the normal fenced Writer, after matching the complete
// batch to the currently assigned activity. Temporal outages never authorize.
func (a FencedAuthority) EmitChildOrigin(ctx context.Context, worker string, request livejournal.ChildOriginEmitRequest) (livejournal.EmitResponse, error) {
	if a.OriginTemporal == nil || a.OriginJournal == nil {
		return livejournal.EmitResponse{}, engine.ErrChildCustodyUnavailable
	}
	store, _, _, err := a.bound(ctx)
	if err != nil {
		return livejournal.EmitResponse{}, classify(err)
	}
	pins, err := store.RunPinsOf(ctx, request.Batch.RunID)
	if err != nil {
		return livejournal.EmitResponse{}, classify(err)
	}
	if err := verifyOriginActivity(ctx, a.OriginTemporal, pins, worker, request); err != nil {
		return livejournal.EmitResponse{}, err
	}
	return a.OriginJournal.Emit(ctx, request.Batch)
}

func verifyOriginActivity(ctx context.Context, temporal OriginTemporal, pins acceptance.RunPins, worker string, request livejournal.ChildOriginEmitRequest) error {
	proof, batch := request.Activity, request.Batch
	if !strings.HasPrefix(worker, "worker:") || strings.TrimPrefix(worker, "worker:") == "" || proof.WorkflowRunID == "" || proof.ActivityID == "" || proof.Attempt < 1 || !batch.HasChildStageStart() || batch.RunID != pins.RunID || batch.Gaggle != pins.Gaggle {
		return engine.ErrChildCustodyRefused
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := verifyOriginLoan(ctx, temporal, pins, worker, proof); err != nil {
		return err
	}
	history := temporal.GetWorkflowHistory(ctx, pins.RunID, proof.WorkflowRunID, false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	matched := false
	for history.HasNext() {
		event, err := history.Next()
		if err != nil {
			return errors.Join(engine.ErrChildCustodyUnavailable, err)
		}
		scheduled := event.GetActivityTaskScheduledEventAttributes()
		if scheduled == nil || scheduled.ActivityId != proof.ActivityID {
			continue
		}
		if scheduled.ActivityType.GetName() != engine.ActEmitJournal {
			return engine.ErrChildCustodyRefused
		}
		var recorded livejournal.EmitRequest
		if err := converter.GetDefaultDataConverter().FromPayloads(scheduled.Input, &recorded); err != nil {
			return errors.Join(engine.ErrChildCustodyRefused, err)
		}
		want, err := json.Marshal(recorded)
		if err != nil {
			return errors.Join(engine.ErrChildCustodyRefused, err)
		}
		got, err := json.Marshal(batch)
		if err != nil || !bytes.Equal(want, got) {
			return engine.ErrChildCustodyRefused
		}
		matched = true
		break
	}
	if !matched {
		return engine.ErrChildCustodyRefused
	}
	// History retrieval may span a reassignment or reset. Re-observe the loan
	// before letting the fenced journal owner commit the recorded batch.
	return verifyOriginLoan(ctx, temporal, pins, worker, proof)
}

func verifyOriginLoan(ctx context.Context, temporal OriginTemporal, pins acceptance.RunPins, worker string, proof livejournal.ChildOriginActivity) error {
	description, err := temporal.DescribeWorkflowExecution(ctx, pins.RunID, "")
	if err != nil {
		return errors.Join(engine.ErrChildCustodyUnavailable, err)
	}
	info := description.GetWorkflowExecutionInfo()
	if info.GetExecution().GetWorkflowId() != pins.RunID || info.GetExecution().GetRunId() != proof.WorkflowRunID || info.GetStatus() != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING || info.GetType().GetName() != "Run" {
		return engine.ErrChildCustodyRefused
	}
	var owner startfence.StartOwnerMemo
	payload := info.GetMemo().GetFields()[startfence.StartOwnerMemoKey]
	if payload == nil || converter.GetDefaultDataConverter().FromPayload(payload, &owner) != nil || owner.InputDigest != pins.InputDigest || owner.AcceptanceID != pins.AcceptanceID {
		return engine.ErrChildCustodyRefused
	}
	for _, pending := range description.GetPendingActivities() {
		if pending.ActivityId == proof.ActivityID && pending.ActivityType.GetName() == engine.ActEmitJournal && pending.Attempt == proof.Attempt && pending.State == enumspb.PENDING_ACTIVITY_STATE_STARTED && pending.LastWorkerIdentity == strings.TrimPrefix(worker, "worker:") {
			return nil
		}
	}
	return engine.ErrChildCustodyRefused
}
