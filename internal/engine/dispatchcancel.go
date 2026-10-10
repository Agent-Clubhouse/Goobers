package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
)

const (
	dispatchCancellationChange   = "dispatch-cancellation-v1"
	dispatchHeartbeatTimeout     = 10 * time.Second
	dispatchHeartbeatInterval    = time.Second
	dispatchSurrenderReadTimeout = 30 * time.Second
)

// Only remote dispatch waits for cancellation cleanup and requires heartbeats.
// Older histories retain their exact options and immediate cancellation result.
func dispatchActivityContext(ctx workflow.Context, limits apiv1.Limits, queue string) workflow.Context {
	options := stageActivityOptions(limits, queue)
	if workflow.GetVersion(ctx, dispatchCancellationChange, workflow.DefaultVersion, 1) != workflow.DefaultVersion {
		options.HeartbeatTimeout = dispatchHeartbeatTimeout
		options.WaitForCancellation = true
	}
	return workflow.WithActivityOptions(ctx, options)
}

// Temporal delivers activity cancellation through heartbeat replies. Keep
// heartbeating until dispatch returns, including its bounded detached cleanup,
// so heartbeat timeout cannot settle the workflow while cleanup is in flight.
// Direct non-Temporal callers already own their context and need no heartbeat.
func heartbeatDispatch(ctx context.Context) func() {
	return heartbeatDispatchDetails(ctx, nil)
}

func heartbeatDispatchDetails(ctx context.Context, details func() any) func() {
	if !activity.IsActivity(ctx) {
		return func() {}
	}
	heartbeatCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	record := func() {
		if details == nil {
			activity.RecordHeartbeat(heartbeatCtx)
		} else {
			activity.RecordHeartbeat(heartbeatCtx, details())
		}
	}
	record()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(dispatchHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				record()
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func (a *Activities) logDispatchCleanup(ctx context.Context, attempt dispatcher.Attempt, report dispatcher.Report) {
	if report.DisposeErr == nil || !activity.IsActivity(ctx) {
		return
	}
	diagnostic := a.scrubber().Scrub([]byte(report.DisposeErr.Error()))
	if len(diagnostic) > 2048 {
		diagnostic = diagnostic[:2048]
	}
	activity.GetLogger(ctx).Warn("dispatch pod cleanup was not confirmed",
		"runId", attempt.RunID, "stage", attempt.Stage, "pod", report.Pod,
		"deleteAccepted", report.Disposed, "error", string(diagnostic))
}

// Waiting for cleanup delivers the actual activity outcome to the workflow.
// Preserve genuine cancellation without changing stage-deadline classification;
// the caller keeps confirmed surrender authoritative before entering this arm.
// A context the server did not cancel was lost to its control path (#6750):
// worker shutdown or a failed heartbeat. The SDK reports a bare
// context.Canceled from such a context as an ordinary policy failure, so it
// is committed as infrastructure carrying the worker-loss marker instead.
func unconfirmedDispatchFailure(ctx context.Context, err error, report dispatcher.Report) (stageActivityResult, error) {
	if errors.Is(ctx.Err(), context.Canceled) {
		if !cancellationRequested(ctx) {
			return dispatchFailureResult(dispatchControlPathLost(context.Cause(ctx), err), report)
		}
		if errors.Is(err, context.Canceled) {
			return stageActivityResult{}, context.Canceled
		}
	}
	return dispatchFailureResult(classifyDispatchError(err), report)
}

// failureTypeWorkerLost marks an attempt whose activity lost its worker or
// heartbeat path. Like a heartbeat timeout, the pod may have run to effect.
const failureTypeWorkerLost = "GoobersWorkerLost"

// Only a server-delivered cancellation carries a Temporal CanceledError cause.
// Direct non-Temporal callers own their context, so any cancel is requested.
func cancellationRequested(ctx context.Context) bool {
	return !activity.IsActivity(ctx) || temporal.IsCanceledError(context.Cause(ctx))
}

func dispatchControlPathLost(cause, err error) error {
	message := fmt.Sprintf("engine: dispatch control path lost (%v): %v", cause, err)
	return temporal.NewApplicationErrorWithOptions(message, FailureTypeInfrastructure, temporal.ApplicationErrorOptions{
		Cause: temporal.NewApplicationError(message, failureTypeWorkerLost),
	})
}

// Once surrender is confirmed, cancellation must not discard the authoritative
// output, including when it arrives during the read. This bounded final read
// has its own 30s bound after disposal (at most 60s together); heartbeats
// remain live throughout both operations.
// Unconfirmed reads keep the activity context and existing failure semantics.
func (a *Activities) readDispatchSurrender(ctx context.Context, attempt dispatcher.Attempt, confirmed bool) (dispatcher.SurrenderedResult, error) {
	if confirmed {
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dispatchSurrenderReadTimeout)
		defer cancel()
		ctx = readCtx
	}
	return dispatcher.ReadSurrenderedResult(ctx, a.Surrenders, attempt.RunID, attempt.Stage, attempt.IdentityAttempt())
}
