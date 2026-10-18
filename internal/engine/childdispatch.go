package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

// ChildDispatchStopSignal requests bounded pod teardown while retaining its outcome.
const ChildDispatchStopSignal = "child-dispatch-stop"

// ChildDispatchInput carries already admitted, immutable claim checks. The
// daemon keeps workspace custody; only an existing dispatch worker creates pods.
type ChildDispatchInput struct {
	Attempt  dispatcher.Attempt
	Eligible []dispatcher.RunnerSpec
	Queue    string
}

// ChildDispatchResult keeps termination proof even when execution failed. Errors
// have a closed wire vocabulary; arbitrary error implementations never enter
// persisted history or disappear during JSON round trips.
type ChildDispatchResult struct {
	BindingDigest  string
	Report         dispatcher.Report
	Failure        string
	DisposalFailed bool
}

// BindingDigest detects identity-key reuse with a changed immutable payload.
func (in ChildDispatchInput) BindingDigest() string {
	in.Attempt.OwningWorkflowID = ""
	data, _ := json.Marshal(in)
	return journal.Digest(data)
}

// ChildDispatchWorkflowID is the durable physical-attempt deduplication identity.
func ChildDispatchWorkflowID(a dispatcher.Attempt) string {
	return "child/" + DispatchOneWorkflowID(a.RunID, a.Stage, a.PodAttempt)
}

// Validate refuses unsupported placements, mutable custody, and embedded credentials.
func (in ChildDispatchInput) Validate() error {
	a := in.Attempt
	if a.RunID == "" || a.Gaggle == "" || a.Stage == "" || a.Number < 1 || a.PodAttempt < 1 || !blobstore.ValidDigest(a.ChildExecutionDigest) || in.Queue == "" || len(in.Eligible) == 0 || len(in.Eligible) > 128 {
		return errors.New("invalid isolated child dispatch identity")
	}
	if a.CheckoutCapability != "" || a.CLIStage || a.WorkspaceDelta != "" || a.SyncBase || a.PodToken != "" || !a.PlaneTokens.Empty() || (a.Agentic && !blobstore.ValidDigest(a.KitDigest)) {
		return errors.New("isolated child dispatch exceeds supported custody")
	}
	for _, candidate := range in.Eligible {
		if candidate.OS != "linux" || candidate.HostKind != instance.RunnerHostImage {
			return errors.New("isolated child dispatch requires Linux image placements")
		}
	}
	data, err := json.Marshal(in)
	if err != nil || len(data) > 2<<20 {
		return errors.New("isolated child dispatch exceeds 2 MiB transport bound")
	}
	return nil
}

// ChildDispatchOne waits for cleanup after stop requests. Retry decisions remain
// with the parent driver; this workflow schedules exactly one activity attempt.
func ChildDispatchOne(ctx workflow.Context, in ChildDispatchInput) (ChildDispatchResult, error) {
	if err := in.Validate(); err != nil {
		return ChildDispatchResult{}, temporal.NewNonRetryableApplicationError(err.Error(), FailureTypeStage, nil)
	}
	if workflow.GetInfo(ctx).WorkflowExecution.ID != ChildDispatchWorkflowID(in.Attempt) {
		return ChildDispatchResult{}, temporal.NewNonRetryableApplicationError("isolated child dispatch identity differs from workflow id", FailureTypeStage, nil)
	}
	in.Attempt.OwningWorkflowID = workflow.GetInfo(ctx).WorkflowExecution.ID
	owned, _ := workflow.NewDisconnectedContext(ctx)
	activityCtx, cancel := workflow.WithCancel(owned)
	defer cancel()
	timeout := in.Attempt.Timeout
	if timeout <= 0 {
		timeout = dispatcher.DefaultStageTimeout
	}
	activityCtx = workflow.WithActivityOptions(activityCtx, workflow.ActivityOptions{TaskQueue: in.Queue,
		StartToCloseTimeout: timeout + 5*time.Minute, ScheduleToStartTimeout: stageScheduleToStart,
		HeartbeatTimeout: dispatchHeartbeatTimeout, WaitForCancellation: true, RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1}})
	future := workflow.ExecuteActivity(activityCtx, "DispatchChildPod", in)
	selector := workflow.NewSelector(owned)
	selector.AddFuture(future, func(workflow.Future) {})
	selector.AddReceive(workflow.GetSignalChannel(ctx, ChildDispatchStopSignal), func(ch workflow.ReceiveChannel, _ bool) { var stop bool; ch.Receive(owned, &stop); cancel() })
	selector.AddReceive(ctx.Done(), func(workflow.ReceiveChannel, bool) { cancel() })
	selector.Select(owned)
	var result ChildDispatchResult
	err := future.Get(owned, &result)
	if err != nil {
		return recoverChildDispatch(owned, in, err)
	}
	return result, nil
}

// DispatchChildPod is registered with the existing worker Activities receiver.
// It never resolves the generated workflow from the mutable named catalog.
func (a *Activities) DispatchChildPod(ctx context.Context, in ChildDispatchInput) (ChildDispatchResult, error) {
	if err := in.Validate(); err != nil {
		return ChildDispatchResult{}, err
	}
	if a.Dispatcher == nil {
		return ChildDispatchResult{}, errors.New("isolated child dispatcher unavailable")
	}
	if a.Canary != nil {
		data, _ := json.Marshal(in)
		if !bytes.Equal(data, a.Canary.Scrub(data)) {
			return ChildDispatchResult{}, errors.New("isolated child dispatch contains registered secret material")
		}
	}
	ctx, stop := heartbeatChildDispatch(ctx, in)
	defer stop()
	report, err := a.Dispatcher.Dispatch(ctx, in.Attempt, in.Eligible)
	return a.finishChildDispatch(ctx, in, report, err)
}

func (a *Activities) finishChildDispatch(ctx context.Context, in ChildDispatchInput, report dispatcher.Report, err error) (ChildDispatchResult, error) {
	a.logDispatchCleanup(ctx, in.Attempt, report)
	out := ChildDispatchResult{BindingDigest: in.BindingDigest(), Report: report, DisposalFailed: report.DisposeErr != nil}
	out.Report.DisposeErr = nil
	switch {
	case err == nil:
	case errors.Is(err, dispatcher.ErrStageFailed):
		out.Failure = "stage_failed"
	case errors.Is(err, dispatcher.ErrChildIsolation):
		out.Failure = "isolation"
	case errors.Is(err, dispatcher.ErrSurrenderUnconfirmed):
		out.Failure = "surrender"
	default:
		out.Failure = "dispatch"
	}
	return a.completeChildDispatch(ctx, out)
}

// DispatchError reconstructs a conservative typed failure from the worker wire
// response. Missing proof is independently refused by the custody consumer.
func (r ChildDispatchResult) DispatchError() error {
	switch r.Failure {
	case "":
		return nil
	case "stage_failed":
		return dispatcher.ErrStageFailed
	case "isolation":
		return dispatcher.ErrChildIsolation
	case "surrender":
		return dispatcher.ErrSurrenderUnconfirmed
	default:
		return fmt.Errorf("isolated child worker dispatch failed")
	}
}
