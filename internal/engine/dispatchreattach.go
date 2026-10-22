package engine

import (
	"errors"
	"fmt"
	"time"

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

type dispatchAttemptDeadlineKey struct{}

func withDispatchAttemptDeadline(ctx workflow.Context, started time.Time, limits *apiv1.Limits) workflow.Context {
	var effective apiv1.Limits
	if limits != nil {
		effective = *limits
	}
	return workflow.WithValue(ctx, dispatchAttemptDeadlineKey{}, started.Add(dispatchAttemptDuration(effective)))
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
	deadline, _ := ctx.Value(dispatchAttemptDeadlineKey{}).(time.Time)
	if deadline.IsZero() {
		deadline = workflow.Now(ctx).Add(dispatchAttemptDuration(input.Envelope.Limits))
	}
	var custody *dispatcher.PodCustody
	for handoffs := 0; ; handoffs++ {
		remaining := deadline.Sub(workflow.Now(ctx))
		if remaining <= 0 {
			return DispatchStageResult{}, temporal.NewApplicationError(
				"engine: stage attempt deadline expired during worker handoff", FailureTypeStage)
		}
		if handoffs > maxDispatchHandoffs {
			message := fmt.Sprintf("engine: worker handoff limit %d exhausted", maxDispatchHandoffs)
			return DispatchStageResult{}, temporal.NewApplicationErrorWithOptions(message, FailureTypeInfrastructure, temporal.ApplicationErrorOptions{
				Cause: temporal.NewApplicationError(message, failureTypeHandoffExhausted),
			})
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
