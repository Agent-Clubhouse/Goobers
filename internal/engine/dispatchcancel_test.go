package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/temporaltest"
)

func TestDispatchCancellationOptionsPreserveLegacyAndLocalStages(t *testing.T) {
	for _, version := range []workflow.Version{workflow.DefaultVersion, 1} {
		var suite testsuite.WorkflowTestSuite
		env := temporaltest.NewWorkflowEnvironment(&suite)
		env.OnGetVersion(dispatchCancellationChange, workflow.DefaultVersion, 1).Return(version)
		env.ExecuteWorkflow(func(ctx workflow.Context) error {
			limits := apiv1.Limits{MaxDurationSeconds: 45}
			local := stageActivityOptions(limits, "queue")
			remote := workflow.GetActivityOptions(dispatchActivityContext(ctx, limits, "queue"))
			if local.WaitForCancellation || local.HeartbeatTimeout != 0 {
				t.Fatal("local activities inherited dispatch heartbeats")
			}
			if remote.StartToCloseTimeout != local.StartToCloseTimeout || remote.ScheduleToStartTimeout != local.ScheduleToStartTimeout || remote.TaskQueue != local.TaskQueue || remote.RetryPolicy.MaximumAttempts != 1 {
				t.Fatal("dispatch changed existing routing/limits/retry ownership")
			}
			if version == workflow.DefaultVersion {
				if remote.WaitForCancellation || remote.HeartbeatTimeout != 0 {
					t.Fatal("legacy history changed cancellation options")
				}
			} else if !remote.WaitForCancellation || remote.HeartbeatTimeout != dispatchHeartbeatTimeout {
				t.Fatal("new dispatch does not await heartbeating cleanup")
			}
			return nil
		})
		if err := env.GetWorkflowError(); err != nil {
			t.Fatal(err)
		}
		env.AssertExpectations(t)
	}
}

func TestReviewerDispatchReceivesCancellationOptions(t *testing.T) {
	in := placedGateInput("cancel-options-review")
	in.Placements = []PinnedPlacement{remoteGatePin()}
	store := surrenderStore(t)
	putSurrendered(t, store, in.RunID, "review", 1, reviewSurrender(apiv1.Verdict{Decision: apiv1.VerdictPass}))
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(&Activities{
		Goober: refusingReviewer(t), Workspaces: testWorkspaces(t),
		Det: &fakeRunner{run: func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
		}},
		Dispatcher: &fakeStageDispatcher{report: dispatcher.Report{SurrenderConfirmed: true}}, Surrenders: store,
	})
	var dispatches int
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, args converter.EncodedValues) {
		if info.ActivityType.Name != ActDispatchStage {
			if info.HeartbeatTimeout != 0 {
				t.Errorf("local %s requires dispatch heartbeats", info.ActivityType.Name)
			}
			return
		}
		dispatches++
		var input DispatchStageInput
		if err := args.Get(&input); err != nil {
			t.Fatal(err)
		}
		if !input.Review || info.HeartbeatTimeout != dispatchHeartbeatTimeout {
			t.Fatalf("review dispatch heartbeat=%s, input=%+v", info.HeartbeatTimeout, input)
		}
	})
	env.ExecuteWorkflow(Run, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if dispatches != 1 {
		t.Fatalf("review dispatch count = %d", dispatches)
	}
}

func TestDispatchCancellationPreservesSurrenderAndDeadlineClassification(t *testing.T) {
	for _, mode := range []string{"canceled", "active", "deadline", "surrendered"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			dispatchErr := context.Canceled
			if mode == "canceled" || mode == "surrendered" {
				cancel()
			}
			if mode == "deadline" {
				expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer stop()
				ctx = expired
				dispatchErr = context.DeadlineExceeded
			}
			store := surrenderStore(t)
			confirmed := mode == "surrendered"
			if confirmed {
				putSurrendered(t, store, "cancel-class", "build", 1, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}})
			}
			acts := &Activities{Surrenders: store, Dispatcher: &fakeStageDispatcher{report: dispatcher.Report{SurrenderConfirmed: confirmed}, err: dispatchErr}}
			result, err := acts.DispatchStage(ctx, dispatchInput("cancel-class", "build", 1))
			switch mode {
			case "surrendered":
				if err != nil || result.Status != apiv1.ResultSuccess {
					t.Fatalf("confirmed surrender lost: %+v, %v", result, err)
				}
			case "canceled":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("actual cancellation = %v", err)
				}
			default:
				var appErr *temporal.ApplicationError
				if !errors.As(err, &appErr) || appErr.Type() != FailureTypeInfrastructure {
					t.Fatalf("existing unconfirmed failure classification changed: %v", err)
				}
			}
		})
	}
}

type cancellationReadPlane struct {
	dispatcher.SurrenderPlane
	beforeGet func(context.Context)
}

func (p cancellationReadPlane) Get(ctx context.Context, run, stage string, attempt int) ([]byte, error) {
	p.beforeGet(ctx)
	return p.SurrenderPlane.Get(ctx, run, stage, attempt)
}

func TestConfirmedSurrenderReadSurvivesCancellationDuringGetWithBound(t *testing.T) {
	type readContextKey struct{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), readContextKey{}, "retained"))
	defer cancel()
	store := surrenderStore(t)
	putSurrendered(t, store, "cancel-read", "build", 1, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}})
	plane := cancellationReadPlane{SurrenderPlane: store, beforeGet: func(readCtx context.Context) {
		cancel()
		deadline, bounded := readCtx.Deadline()
		if readCtx.Err() != nil || !bounded || time.Until(deadline) > dispatchSurrenderReadTimeout || readCtx.Value(readContextKey{}) != "retained" {
			t.Fatal("confirmed surrender read lost its independent deadline")
		}
	}}
	acts := &Activities{Surrenders: plane, Dispatcher: &fakeStageDispatcher{report: dispatcher.Report{SurrenderConfirmed: true}}}
	result, err := acts.DispatchStage(ctx, dispatchInput("cancel-read", "build", 1))
	if err != nil || result.Status != apiv1.ResultSuccess {
		t.Fatalf("confirmed output lost during read: %+v, %v", result, err)
	}
}
