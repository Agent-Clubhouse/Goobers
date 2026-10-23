package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
)

func TestStaticBranchFailedAdoptionFailsClosedAcrossPolicies(t *testing.T) {
	for _, policy := range []apiv1.BranchFailurePolicy{apiv1.BranchContinueOnError, apiv1.BranchAllOrNothing, apiv1.BranchFailFast} {
		t.Run(string(policy), func(t *testing.T) {
			in := schedulerInput("failed-adoption-"+string(policy), 1)
			in.Run.Placements = []PinnedPlacement{remotePin("alpha")}
			in.Run.Spec.Parallels[0].FailurePolicy = policy
			if policy != apiv1.BranchContinueOnError {
				in.Run.Spec.Parallels[0].OnFailure = wf.TargetAbort
			}
			// An uncertain side effect cannot be retried as a new stage attempt.
			in.Run.Spec.Tasks[1].PolicyActions = []string{"comment-on-pr-advisory"}
			in.Run.Spec.Tasks[1].Capabilities = []string{"github:pr:write"}
			writer, dir := newLiveWriter(t)
			acts := &Activities{Journal: writer, Workspaces: testWorkspaces(t), Det: &fakeRunner{run: func(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
				return recoveryResult(stageOfTaskID(env.TaskID)), nil
			}}}
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			env.RegisterActivity(acts)
			calls := 0
			env.OnActivity(ActDispatchStage, mock.Anything, mock.Anything).Return(func(_ context.Context, input DispatchStageInput) (DispatchStageResult, error) {
				calls++
				if input.Envelope.Attempt != 1 {
					t.Errorf("adoption charged a new attempt: %d", input.Envelope.Attempt)
				}
				if calls == 1 {
					return DispatchStageResult{}, dispatchWorkerHandoff(recoveryCustody())
				}
				if input.Custody == nil || *input.Custody != recoveryCustody() {
					t.Error("adoption lost custody")
				}
				return DispatchStageResult{}, temporal.NewApplicationError("pod UID changed", FailureTypeInfrastructure)
			})
			env.ExecuteWorkflow(staticScheduler, in)
			out := recoveryWorkflowResult(t, env)
			if calls != 2 || !strings.Contains(out.Error, "refusing to retry after worker loss") {
				t.Fatalf("calls=%d status=%s error=%s", calls, out.Result.Status, out.Error)
			}
			events := liveEvents(t, dir, in.Run.RunID)
			assertSchedulerCompleteness(t, events, []journal.BranchStatus{journal.BranchFailed, journal.BranchCancelled, journal.BranchCancelled}, "")
			starts := 0
			for _, event := range events {
				if event.Type == journal.EventStageStarted && event.Stage == "alpha" {
					starts++
				}
			}
			if starts != 1 {
				t.Fatalf("failed adoption created %d stage starts", starts)
			}
		})
	}
}

func recoveryCustody() dispatcher.PodCustody {
	return dispatcher.PodCustody{Namespace: "gaggle-web", Pod: "alpha", UID: "alpha-uid", Attempt: 1, Custody: true}
}

func recoveryWorkflowResult(t *testing.T, env *testsuite.TestWorkflowEnvironment) staticSchedulerResult {
	t.Helper()
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var out staticSchedulerResult
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestConformanceStaticBranchAdoptedFailureFollowsPolicy(t *testing.T) {
	for _, policy := range []apiv1.BranchFailurePolicy{apiv1.BranchContinueOnError, apiv1.BranchAllOrNothing, apiv1.BranchFailFast} {
		t.Run(string(policy), func(t *testing.T) {
			in := schedulerInput("adopted-failure-"+string(policy), 1)
			in.Run.Placements = []PinnedPlacement{remotePin("alpha")}
			in.Run.Spec.Parallels[0].FailurePolicy = policy
			if policy != apiv1.BranchContinueOnError {
				in.Run.Spec.Parallels[0].OnFailure = wf.TargetAbort
			}
			writer, dir := newLiveWriter(t)
			acts := &Activities{Journal: writer, Workspaces: testWorkspaces(t), Det: &fakeRunner{run: func(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
				return recoveryResult(stageOfTaskID(env.TaskID)), nil
			}}}
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			env.RegisterActivity(acts)
			calls := 0
			failure := fail("tool_failed", "adopted stage failed")
			env.OnActivity(ActDispatchStage, mock.Anything, mock.Anything).Return(func(_ context.Context, input DispatchStageInput) (DispatchStageResult, error) {
				calls++
				if calls == 1 {
					return DispatchStageResult{}, dispatchWorkerHandoff(recoveryCustody())
				}
				if input.Custody == nil || *input.Custody != recoveryCustody() || input.Envelope.Attempt != 1 {
					t.Error("adopted failure changed custody/attempt")
				}
				return DispatchStageResult{ResultEnvelope: failure.result}, nil
			})
			env.ExecuteWorkflow(staticScheduler, in)
			out := recoveryWorkflowResult(t, env)
			status, route := StatusBlocked, wf.TargetAbort
			if policy == apiv1.BranchContinueOnError {
				status, route = StatusCompleted, "join"
			}
			if calls != 2 || out.Error != "" || out.Result.Status != status {
				t.Fatalf("calls=%d status=%s error=%s", calls, out.Result.Status, out.Error)
			}
			sibling := journal.BranchSucceeded
			if policy == apiv1.BranchFailFast {
				sibling = journal.BranchCancelled
			}
			want := []journal.BranchStatus{journal.BranchFailed, sibling, sibling}
			assertSchedulerCompleteness(t, liveEvents(t, dir, in.Run.RunID), want, route)
			local := runLocalFixture(t, conformanceFixture{spec: in.Run.Spec, script: map[string][]scriptedCall{
				"seed": {succeed(recoveryResult("seed").Outputs)}, "alpha": {failure},
				"beta": {succeed(recoveryResult("beta").Outputs)}, "gamma": {succeed(recoveryResult("gamma").Outputs)}, "join": {succeed(nil)},
			}}, "local-adopted-failure-"+string(policy))
			assertSchedulerCompleteness(t, local, want, route)
		})
	}
}
