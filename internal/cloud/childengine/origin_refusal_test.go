package childengine

import (
	"context"
	"errors"
	"testing"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/workflowservice/v1"

	"github.com/goobers/goobers/internal/engine"
)

type originChangingLoanTemporal struct {
	*originTemporalFake
	describes    int
	failDescribe bool
	revokeSecond bool
}

func (f *originChangingLoanTemporal) DescribeWorkflowExecution(context.Context, string, string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	f.describes++
	if f.failDescribe {
		return nil, errors.New("describe unavailable")
	}
	if f.revokeSecond && f.describes == 2 {
		f.description.PendingActivities = nil
	}
	return f.description, nil
}

func TestChildOriginWholeBatchAndLoanRefusals(t *testing.T) {
	for _, scenario := range []string{"open", "nonchild-before", "nonchild-after", "operation-order", "operation-key", "continuation", "branch", "gaggle", "empty-worker", "completed", "wrong-workflow-type", "wrong-activity-type", "wrong-activity-id", "wrong-acceptance", "missing-memo", "missing-pending", "cancel-pending", "missing-history", "invalid-payload", "wrong-scheduled-type", "describe-error", "revoked-during-history"} {
		t.Run(scenario, func(t *testing.T) {
			fake, pins, request := originActivityFixture(t)
			temporal := &originChangingLoanTemporal{originTemporalFake: fake}
			worker := "worker:resident-1"
			switch scenario {
			case "open":
				request.Batch.Open.Identity.Workflow = "forged"
			case "nonchild-before":
				request.Batch.Ops[0].Event.Status = "forged"
			case "nonchild-after":
				request.Batch.Ops[2].Event.Runner["kind"] = "forged"
			case "operation-order":
				request.Batch.Ops[0], request.Batch.Ops[2] = request.Batch.Ops[2], request.Batch.Ops[0]
			case "operation-key":
				request.Batch.Ops[1].Key = "forged"
			case "continuation":
				request.Batch.Ops[1].ChildStageStart.Continuation = true
			case "branch":
				request.Batch.Ops[1].Branch = 9
			case "gaggle":
				request.Batch.Gaggle = "other"
			case "empty-worker":
				worker = "worker:"
			case "completed":
				fake.description.WorkflowExecutionInfo.Status = enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED
			case "wrong-workflow-type":
				fake.description.WorkflowExecutionInfo.Type.Name = "other"
			case "wrong-activity-type":
				fake.description.PendingActivities[0].ActivityType.Name = "other"
			case "wrong-activity-id":
				request.Activity.ActivityID = "other"
			case "wrong-acceptance":
				pins.AcceptanceID = "other"
			case "missing-memo":
				fake.description.WorkflowExecutionInfo.Memo = nil
			case "missing-pending":
				fake.description.PendingActivities = nil
			case "cancel-pending":
				fake.description.PendingActivities[0].State = enumspb.PENDING_ACTIVITY_STATE_CANCEL_REQUESTED
			case "missing-history":
				fake.events = nil
			case "invalid-payload":
				fake.events[0].GetActivityTaskScheduledEventAttributes().Input = &commonpb.Payloads{Payloads: []*commonpb.Payload{{Metadata: map[string][]byte{"encoding": []byte("json/plain")}, Data: []byte("broken")}}}
			case "wrong-scheduled-type":
				fake.events[0].GetActivityTaskScheduledEventAttributes().ActivityType.Name = "other"
			case "describe-error":
				temporal.failDescribe = true
			case "revoked-during-history":
				temporal.revokeSecond = true
			}
			err := verifyOriginActivity(t.Context(), temporal, pins, worker, request)
			want := engine.ErrChildCustodyRefused
			if scenario == "describe-error" {
				want = engine.ErrChildCustodyUnavailable
			}
			if !errors.Is(err, want) {
				t.Fatalf("got %v want %v", err, want)
			}
		})
	}
}

func TestChildOriginAcceptsCurrentRetryAttempt(t *testing.T) {
	fake, pins, request := originActivityFixture(t)
	request.Activity.Attempt = 2
	fake.description.PendingActivities[0].Attempt = 2
	if err := verifyOriginActivity(t.Context(), fake, pins, "worker:resident-1", request); err != nil {
		t.Fatal(err)
	}
}
