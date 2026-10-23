package engine

// Ungraceful dispatch loss must preserve custody and the branch attempt, or
// refuse side-effect retries when the last heartbeat carries no custody.

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
)

func TestStaticBranchHardLossWithCustodyAdoptsSameAttempt(t *testing.T) {
	in := schedulerInput("hard-loss-custody", 1)
	in.Run.Placements = []PinnedPlacement{remotePin("alpha")}
	in.Run.Spec.Tasks[1].PolicyActions = []string{"comment-on-pr-advisory"}
	in.Run.Spec.Tasks[1].Capabilities = []string{"github:pr:write"}
	writer, dir := newLiveWriter(t)
	acts := &Activities{Journal: writer, Workspaces: testWorkspaces(t), Det: &fakeRunner{run: func(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		return recoveryResult(stageOfTaskID(env.TaskID)), nil
	}}}
	custody := recoveryCustody()
	details := dispatchHeartbeatDetails{Namespace: custody.Namespace, Pod: custody.Pod, UID: custody.UID, Attempt: custody.Attempt, Custody: true}
	var inputs []DispatchStageInput
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(acts)
	env.OnActivity(ActDispatchStage, mock.Anything, mock.Anything).Return(func(_ context.Context, input DispatchStageInput) (DispatchStageResult, error) {
		inputs = append(inputs, input)
		if len(inputs) == 1 {
			return DispatchStageResult{}, temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_HEARTBEAT, nil, details)
		}
		return DispatchStageResult{ResultEnvelope: recoveryResult("alpha")}, nil
	})
	env.ExecuteWorkflow(staticScheduler, in)
	out := recoveryWorkflowResult(t, env)
	if out.Error != "" || out.Result.Status != StatusCompleted {
		t.Fatalf("hard loss with custody: status=%s error=%s", out.Result.Status, out.Error)
	}
	if len(inputs) != 2 || inputs[1].Custody == nil || *inputs[1].Custody != custody || inputs[1].Envelope.Attempt != inputs[0].Envelope.Attempt || inputs[1].Envelope.TaskID != inputs[0].Envelope.TaskID || inputs[1].Class != inputs[0].Class {
		t.Fatalf("hard loss did not adopt the same attempt/custody: %+v", inputs)
	}
	events := liveEvents(t, dir, in.Run.RunID)
	starts := 0
	for _, event := range events {
		if event.Type == journal.EventStageStarted && event.Stage == "alpha" {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("hard-loss adoption created %d alpha starts", starts)
	}
}

func TestStaticBranchHardLossWithoutCustodyFailsClosed(t *testing.T) {
	for _, policy := range []apiv1.BranchFailurePolicy{apiv1.BranchContinueOnError, apiv1.BranchAllOrNothing, apiv1.BranchFailFast} {
		t.Run(string(policy), func(t *testing.T) {
			in := schedulerInput("hard-loss-no-custody-"+string(policy), 1)
			in.Run.Placements = []PinnedPlacement{remotePin("alpha")}
			in.Run.Spec.Parallels[0].FailurePolicy = policy
			if policy != apiv1.BranchContinueOnError {
				in.Run.Spec.Parallels[0].OnFailure = wf.TargetAbort
			}
			in.Run.Spec.Tasks[1].PolicyActions = []string{"comment-on-pr-advisory"}
			in.Run.Spec.Tasks[1].Capabilities = []string{"github:pr:write"}
			writer, dir := newLiveWriter(t)
			acts := &Activities{Journal: writer, Workspaces: testWorkspaces(t), Det: &fakeRunner{run: func(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
				return recoveryResult(stageOfTaskID(env.TaskID)), nil
			}}}
			calls := 0
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			env.RegisterActivity(acts)
			env.OnActivity(ActDispatchStage, mock.Anything, mock.Anything).Return(func(_ context.Context, _ DispatchStageInput) (DispatchStageResult, error) {
				calls++
				return DispatchStageResult{}, temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_HEARTBEAT, nil)
			})
			env.ExecuteWorkflow(staticScheduler, in)
			out := recoveryWorkflowResult(t, env)
			if calls != 1 || !strings.Contains(out.Error, "refusing to retry after worker loss") {
				t.Fatalf("side-effecting hard loss: calls=%d status=%s error=%s", calls, out.Result.Status, out.Error)
			}
			starts := 0
			for _, event := range liveEvents(t, dir, in.Run.RunID) {
				if event.Type == journal.EventStageStarted && event.Stage == "alpha" {
					starts++
				}
			}
			if starts != 1 {
				t.Fatalf("hard loss created %d alpha starts", starts)
			}
		})
	}
}
