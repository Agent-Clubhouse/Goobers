package engine

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	temporalworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/encoding/protojson"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
)

func TestHandoffDoesNotChargeAttempt(t *testing.T) {
	in := runInput("handoff-same-attempt", apiv1.WorkflowSpec{
		Gaggle: "web", Start: "build", Tasks: []apiv1.Task{podTask("build", "", nil)},
	})
	in.Placements = []PinnedPlacement{remotePin("build")}
	in.Spec.Tasks[0].PolicyActions = []string{"comment-on-pr-advisory"}
	in.Spec.Tasks[0].Capabilities = []string{"github:pr:write"}
	custody := dispatcher.PodCustody{Namespace: "gaggle-web", Pod: "build", UID: "uid-1", Attempt: 1, Custody: true}
	var inputs []DispatchStageInput
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(&Activities{})
	env.OnActivity(ActDispatchStage, mock.Anything, mock.Anything).Return(func(_ context.Context, input DispatchStageInput) (stageActivityResult, error) {
		inputs = append(inputs, input)
		if len(inputs) == 1 {
			return stageActivityResult{}, dispatchWorkerHandoff(custody)
		}
		return stageActivityResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}}, nil
	})
	env.ExecuteWorkflow(Run, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 2 || inputs[0].Envelope.Attempt != 1 || inputs[1].Envelope.Attempt != 1 ||
		inputs[1].Custody == nil || inputs[1].Custody.UID != custody.UID ||
		inputs[0].Class != "" || inputs[1].Class != "" {
		t.Fatalf("handoff dispatches = %+v", inputs)
	}
}

func TestReattachPreservesAttemptDeadline(t *testing.T) {
	var first, second time.Time
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.ExecuteWorkflow(func(ctx workflow.Context) error {
		ctx = dispatchActivityContext(ctx, apiv1.Limits{MaxDurationSeconds: 60}, "queue")
		deadline := workflow.Now(ctx).Add(time.Minute)
		firstCtx := dispatchReattachActivityContext(ctx, deadline.Sub(workflow.Now(ctx)))
		first = workflow.Now(ctx).Add(workflow.GetActivityOptions(firstCtx).StartToCloseTimeout)
		if err := workflow.Sleep(ctx, 10*time.Second); err != nil {
			return err
		}
		secondCtx := dispatchReattachActivityContext(ctx, deadline.Sub(workflow.Now(ctx)))
		second = workflow.Now(ctx).Add(workflow.GetActivityOptions(secondCtx).StartToCloseTimeout)
		return nil
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if !first.Equal(second) {
		t.Fatalf("activity deadline reset across handoff: first=%s second=%s", first, second)
	}
}

func TestHandoffCapIsInfra(t *testing.T) {
	custody := dispatcher.PodCustody{Namespace: "gaggle-web", Pod: "build", UID: "uid-1", Attempt: 1, Custody: true}
	calls := 0
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(&Activities{})
	env.OnActivity(ActDispatchStage, mock.Anything, mock.Anything).Return(func(_ context.Context, _ DispatchStageInput) (stageActivityResult, error) {
		calls++
		return stageActivityResult{}, dispatchWorkerHandoff(custody)
	})
	var class journal.AttemptClass
	var classifyErr error
	env.ExecuteWorkflow(func(ctx workflow.Context) error {
		ctx = dispatchActivityContext(ctx, apiv1.Limits{MaxDurationSeconds: 60}, "queue")
		_, err := executeDispatchActivity(ctx, dispatchInput("handoff-cap", "build", 1))
		class, classifyErr = ClassifyDispatchFailure(err)
		return nil
	})
	err := env.GetWorkflowError()
	if err != nil || calls != maxDispatchHandoffs+1 {
		t.Fatalf("handoff cap: calls=%d err=%v", calls, err)
	}
	if classifyErr != nil || class != journal.AttemptInfra {
		t.Fatalf("handoff exhaustion class = %q, %v", class, classifyErr)
	}
}

func TestDispatchReattachVersionGateReplaysPreChangeHistory(t *testing.T) {
	data, err := os.ReadFile("testdata/dispatchstage_history.json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), dispatchReattachChange) {
		t.Fatal("pre-change fixture already contains the reattach marker")
	}
	history := &historypb.History{}
	if err := protojson.Unmarshal(data, history); err != nil {
		t.Fatal(err)
	}
	replayer := temporalworker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(Run)
	if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
		t.Fatalf("pre-reattach history does not replay through %s: %v", dispatchReattachChange, err)
	}
}

func TestDispatchHandoffDetailsRequireCustody(t *testing.T) {
	for _, err := range []error{
		temporal.NewApplicationError("worker-handoff", failureTypeWorkerHandoff),
		dispatchWorkerHandoff(dispatcher.PodCustody{}),
	} {
		if _, ok := dispatchHandoffCustody(err); ok {
			t.Fatalf("invalid custody accepted: %v", err)
		}
	}
	if _, ok := dispatchHandoffCustody(errors.New("ordinary failure")); ok {
		t.Fatal("ordinary failure classified as handoff")
	}
}

func TestHandoffFailuresRefuseSideEffectingRetry(t *testing.T) {
	custody := dispatcher.PodCustody{Namespace: "gaggle-web", Pod: "build", UID: "uid-1", Attempt: 1, Custody: true}
	for _, scenario := range []string{"pod-missing", "uid-mismatch", "get-pod-error", "schedule-to-start", "cap", "deadline"} {
		t.Run(scenario, func(t *testing.T) {
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			env.RegisterActivity(&Activities{})
			activityCalls := 0
			env.OnActivity(ActDispatchStage, mock.Anything, mock.Anything).Return(func(_ context.Context, _ DispatchStageInput) (stageActivityResult, error) {
				activityCalls++
				if scenario == "cap" || activityCalls == 1 {
					return stageActivityResult{}, dispatchWorkerHandoff(custody)
				}
				if scenario == "schedule-to-start" {
					return stageActivityResult{}, temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_SCHEDULE_TO_START, nil)
				}
				return stageActivityResult{}, temporal.NewApplicationError(scenario, FailureTypeInfrastructure)
			})
			var attempts []int
			env.ExecuteWorkflow(func(ctx workflow.Context) error {
				task := retrySpec(nil).Tasks[0]
				task.PolicyActions = []string{"comment-on-pr-advisory"}
				ctx = dispatchActivityContext(ctx, apiv1.Limits{MaxDurationSeconds: 60}, "queue")
				_, err := dispatchWithRetry(ctx, RunInput{}, task, &runJournal{}, nil, func(ctx workflow.Context, attempt int, _ journal.AttemptClass) (stageActivityResult, error) {
					attempts = append(attempts, attempt)
					input := dispatchInput("side-effect", "build", attempt)
					input.Envelope.Limits.MaxDurationSeconds = 60
					if scenario == "deadline" {
						input.Custody = &custody
						ctx = withDispatchAttemptStarted(ctx, workflow.Now(ctx).Add(-time.Hour))
					}
					return executeDispatchActivity(ctx, input)
				}, nil)
				return err
			})
			err := env.GetWorkflowError()
			if err == nil || !strings.Contains(err.Error(), "refusing to retry after worker loss") {
				t.Fatalf("%s side-effecting failure = %v", scenario, err)
			}
			if len(attempts) != 1 || attempts[0] != 1 {
				t.Fatalf("%s attempts = %v, want only attempt 1", scenario, attempts)
			}
			if scenario == "cap" && activityCalls != maxDispatchHandoffs+1 {
				t.Fatalf("cap activity calls = %d", activityCalls)
			}
		})
	}
}

func TestWorkerShutdownDecisionRejectsOtherCancellationCauses(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "worker shutdown", err: temporalworker.ErrWorkerShutdown, want: true},
		{name: "ordinary cancel", err: context.Canceled},
		{name: "deadline", err: context.DeadlineExceeded},
		{name: "heartbeat not found", err: errors.New("activity heartbeat not found")},
		{name: "namespace not active", err: errors.New("namespace not active")},
		{name: "activity paused", err: errors.New("activity paused")},
		{name: "activity reset", err: errors.New("activity reset")},
		{name: "heartbeat retry exhausted", err: errors.New("heartbeat rpc unavailable")},
		{name: "server cancellation", err: temporal.NewCanceledError()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			cancel(tc.err)
			if got := workerShutdownRequested(ctx); got != tc.want {
				t.Fatalf("workerShutdownRequested(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(temporalworker.ErrWorkerShutdown)
	var custody atomic.Pointer[dispatcher.PodCustody]
	if workerHandoffWithCustody(ctx, &custody) {
		t.Fatal("child/no-custody shutdown took ordinary handoff path")
	}
	custody.Store(&dispatcher.PodCustody{UID: "uid", Custody: true})
	if !workerHandoffWithCustody(ctx, &custody) {
		t.Fatal("worker shutdown with recorded custody was not handed off")
	}
}

func TestReattachDeadlineUsesEffectiveEnvelopeTimeout(t *testing.T) {
	task := podTask("build", "", nil)
	task.TimeoutSeconds = 600
	in := runInput("handoff-timeout-seconds", apiv1.WorkflowSpec{Gaggle: "web", Start: "build", Tasks: []apiv1.Task{task}})
	in.Placements = []PinnedPlacement{remotePin("build")}
	custody := dispatcher.PodCustody{Namespace: "gaggle-web", Pod: "build", UID: "uid-1", Attempt: 1, Custody: true}
	var limits []int32
	var windows []time.Duration
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(&Activities{})
	env.OnActivity(ActDispatchStage, mock.Anything, mock.Anything).Return(func(ctx context.Context, input DispatchStageInput) (stageActivityResult, error) {
		limits = append(limits, input.Envelope.Limits.MaxDurationSeconds)
		info := activity.GetInfo(ctx)
		windows = append(windows, info.Deadline.Sub(info.StartedTime))
		if len(limits) == 1 {
			return stageActivityResult{}, dispatchWorkerHandoff(custody)
		}
		return stageActivityResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}}, nil
	})
	env.ExecuteWorkflow(Run, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if len(limits) != 2 || limits[0] != 600 || limits[1] != 600 ||
		windows[1] > 10*time.Minute+stageTimeoutGrace+time.Second {
		t.Fatalf("effective limits=%v windows=%v", limits, windows)
	}
}

func TestReattachCollectsWithinDeadlineGrace(t *testing.T) {
	custody := dispatcher.PodCustody{Namespace: "gaggle-web", Pod: "build", UID: "uid-1", Attempt: 1, Custody: true}
	calls := 0
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(&Activities{})
	env.OnActivity(ActDispatchStage, mock.Anything, mock.Anything).Return(func(_ context.Context, _ DispatchStageInput) (stageActivityResult, error) {
		calls++
		return stageActivityResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}}, nil
	})
	env.ExecuteWorkflow(func(ctx workflow.Context) error {
		ctx = dispatchActivityContext(ctx, apiv1.Limits{MaxDurationSeconds: 60}, "queue")
		ctx = withDispatchAttemptStarted(ctx, workflow.Now(ctx).Add(-61*time.Second))
		input := dispatchInput("grace", "build", 1)
		input.Envelope.Limits.MaxDurationSeconds = 60
		input.Custody = &custody
		_, err := executeDispatchActivity(ctx, input)
		return err
	})
	if err := env.GetWorkflowError(); err != nil || calls != 1 {
		t.Fatalf("grace adoption calls=%d err=%v", calls, err)
	}
}

func TestHeartbeatTimeoutReattachesFromLastDetails(t *testing.T) {
	custody := dispatcher.PodCustody{Namespace: "gaggle-web", Pod: "build", UID: "uid-1", Attempt: 1, Custody: true}
	details := dispatchHeartbeatDetails{
		Namespace: custody.Namespace, Pod: custody.Pod, UID: custody.UID, Attempt: custody.Attempt, Custody: true,
	}
	var inputs []DispatchStageInput
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(&Activities{})
	env.OnActivity(ActDispatchStage, mock.Anything, mock.Anything).Return(func(_ context.Context, input DispatchStageInput) (stageActivityResult, error) {
		inputs = append(inputs, input)
		if len(inputs) == 1 {
			return stageActivityResult{}, temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_HEARTBEAT, nil, details)
		}
		return stageActivityResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}}, nil
	})
	env.ExecuteWorkflow(func(ctx workflow.Context) error {
		ctx = dispatchActivityContext(ctx, apiv1.Limits{MaxDurationSeconds: 60}, "queue")
		_, err := executeDispatchActivity(ctx, dispatchInput("hard-loss", "build", 1))
		return err
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 2 || inputs[1].Custody == nil || inputs[1].Custody.UID != custody.UID {
		t.Fatalf("heartbeat custody was not adopted: %+v", inputs)
	}
}
