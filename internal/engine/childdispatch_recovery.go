package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/dispatcher"
)

// ChildDispatchCustody binds the last durable heartbeat to its immutable input.
// Empty or absent custody is deliberately not recoverable by guessed pod name.
type ChildDispatchCustody struct {
	BindingDigest string
	Pod           dispatcher.ChildPodCustody
}

func heartbeatChildDispatch(ctx context.Context, in ChildDispatchInput) (context.Context, func()) {
	var custody atomic.Pointer[ChildDispatchCustody]
	ctx = dispatcher.WithChildCustodyObserver(ctx, func(pod dispatcher.ChildPodCustody) {
		custody.Store(&ChildDispatchCustody{BindingDigest: in.BindingDigest(), Pod: pod})
	})
	return ctx, heartbeatDispatchDetails(ctx, func() any { return custody.Load() })
}

func recoverChildDispatch(ctx workflow.Context, in ChildDispatchInput, dispatchErr error) (ChildDispatchResult, error) {
	if workflow.GetVersion(ctx, "isolated-child-custody-recovery-v1", workflow.DefaultVersion, 1) == workflow.DefaultVersion {
		return ChildDispatchResult{}, dispatchErr
	}
	var timeout *temporal.TimeoutError
	var custody ChildDispatchCustody
	if !errors.As(dispatchErr, &timeout) || timeout.TimeoutType() != enumspb.TIMEOUT_TYPE_HEARTBEAT || !timeout.HasLastHeartbeatDetails() || timeout.LastHeartbeatDetails(&custody) != nil || custody.BindingDigest != in.BindingDigest() || custody.Pod.UID == "" {
		return ChildDispatchResult{}, dispatchErr
	}
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{TaskQueue: in.Queue, StartToCloseTimeout: 5 * time.Minute, HeartbeatTimeout: dispatchHeartbeatTimeout, WaitForCancellation: true, RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1}})
	var result ChildDispatchResult
	err := workflow.ExecuteActivity(ctx, "ReconcileChildPod", in, custody).Get(ctx, &result)
	return result, err
}

// ReconcileChildPod has no dispatch or create fallback. Only an exact custody
// heartbeat recorded by the previous activity enables observation/teardown.
func (a *Activities) ReconcileChildPod(ctx context.Context, in ChildDispatchInput, custody ChildDispatchCustody) (ChildDispatchResult, error) {
	if err := in.Validate(); err != nil {
		return ChildDispatchResult{}, err
	}
	if custody.BindingDigest != in.BindingDigest() || custody.Pod.UID == "" {
		return ChildDispatchResult{}, errors.New("child recovery custody differs from retained input")
	}
	reconciler, ok := a.Dispatcher.(interface {
		ReconcileChildPod(context.Context, dispatcher.Attempt, dispatcher.ChildPodCustody) (dispatcher.Report, error)
	})
	if !ok {
		return ChildDispatchResult{}, errors.New("child dispatcher recovery unavailable")
	}
	stop := heartbeatDispatch(ctx)
	defer stop()
	report, err := reconciler.ReconcileChildPod(ctx, in.Attempt, custody.Pod)
	return a.finishChildDispatch(ctx, in, report, err)
}
