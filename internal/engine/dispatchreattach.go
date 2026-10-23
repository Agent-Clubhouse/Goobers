package engine

import (
	"errors"
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
)

const (
	dispatchReattachChange      = "dispatch-reattach-v1"
	maxDispatchHandoffs         = 5
	failureTypeHandoffExhausted = "GoobersHandoffExhausted"
)

type dispatchAttemptStartedKey struct{}

func withDispatchAttemptStarted(ctx workflow.Context, started time.Time) workflow.Context {
	return workflow.WithValue(ctx, dispatchAttemptStartedKey{}, started)
}

func dispatchAttemptDuration(limits apiv1.Limits) time.Duration {
	if limits.MaxDurationSeconds > 0 {
		return time.Duration(limits.MaxDurationSeconds) * time.Second
	}
	return activityTimeout
}

func executeDispatchActivity(ctx workflow.Context, input DispatchStageInput) (DispatchStageResult, error) {
	if workflow.GetVersion(ctx, dispatchReattachChange, workflow.DefaultVersion, 1) == workflow.DefaultVersion {
		var result DispatchStageResult
		err := workflow.ExecuteActivity(ctx, ActDispatchStage, input).Get(ctx, &result)
		return result, err
	}
	started, _ := ctx.Value(dispatchAttemptStartedKey{}).(time.Time)
	if started.IsZero() {
		started = workflow.Now(ctx)
	}
	deadline := started.Add(dispatchAttemptDuration(input.Envelope.Limits))
	custody := input.Custody
	handoffs := 0
	if custody != nil {
		handoffs = 1
	}
	for ; ; handoffs++ {
		remaining := deadline.Sub(workflow.Now(ctx))
		if remaining+stageTimeoutGrace <= 0 {
			timeout := temporal.NewApplicationError("engine: stage attempt deadline and grace expired during worker handoff", FailureTypeStage)
			return DispatchStageResult{}, dispatchHandoffWorkerLoss(timeout)
		}
		if handoffs > maxDispatchHandoffs {
			message := fmt.Sprintf("engine: worker handoff limit %d exhausted", maxDispatchHandoffs)
			exhausted := temporal.NewApplicationError(message, failureTypeHandoffExhausted)
			return DispatchStageResult{}, dispatchHandoffWorkerLoss(exhausted)
		}
		attemptCtx := ctx
		if handoffs > 0 {
			attemptCtx = dispatchReattachActivityContext(ctx, remaining)
		}
		input.Custody = custody
		var result DispatchStageResult
		err := workflow.ExecuteActivity(attemptCtx, ActDispatchStage, input).Get(attemptCtx, &result)
		next, handoff := dispatchHandoffCustody(err)
		if !handoff {
			if custody != nil && err != nil {
				return result, dispatchHandoffWorkerLoss(err)
			}
			return result, err
		}
		custody = next
	}
}

func dispatchReattachActivityContext(ctx workflow.Context, remaining time.Duration) workflow.Context {
	options := workflow.GetActivityOptions(ctx)
	options.StartToCloseTimeout = remaining + stageTimeoutGrace
	return workflow.WithActivityOptions(ctx, options)
}

func dispatchHandoffCustody(err error) (*dispatcher.PodCustody, bool) {
	var timeout *temporal.TimeoutError
	if errors.As(err, &timeout) && timeout.TimeoutType() == enumspb.TIMEOUT_TYPE_HEARTBEAT && timeout.HasLastHeartbeatDetails() {
		var details dispatchHeartbeatDetails
		if timeout.LastHeartbeatDetails(&details) == nil && details.UID != "" && details.Custody {
			return &dispatcher.PodCustody{
				Namespace: details.Namespace,
				Pod:       details.Pod,
				UID:       details.UID,
				Attempt:   details.Attempt,
				Custody:   true,
			}, true
		}
	}
	for current := err; current != nil; current = errors.Unwrap(current) {
		var appErr *temporal.ApplicationError
		if !errors.As(current, &appErr) {
			return nil, false
		}
		if appErr.Type() == failureTypeWorkerHandoff {
			var custody dispatcher.PodCustody
			if !appErr.HasDetails() || appErr.Details(&custody) != nil || custody.UID == "" || !custody.Custody {
				return nil, false
			}
			return &custody, true
		}
		current = appErr
	}
	return nil, false
}
