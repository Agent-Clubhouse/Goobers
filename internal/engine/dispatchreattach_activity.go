package engine

import (
	"context"
	"errors"
	"sync/atomic"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/worker"

	"github.com/goobers/goobers/internal/dispatcher"
)

func dispatchCustodyContext(ctx context.Context, input DispatchStageInput) (context.Context, *atomic.Pointer[dispatcher.PodCustody], func()) {
	var custody atomic.Pointer[dispatcher.PodCustody]
	if input.Custody != nil {
		initial := *input.Custody
		custody.Store(&initial)
	}
	ctx = dispatcher.WithPodCustodyObserver(ctx, func(observed dispatcher.PodCustody) {
		copy := observed
		custody.Store(&copy)
	})
	ctx = dispatcher.WithWorkerHandoffDecision(ctx, func() bool {
		return workerHandoffWithCustody(ctx, &custody)
	})
	return ctx, &custody, heartbeatDispatchCustody(ctx, custody.Load)
}

func workerHandoffWithCustody(ctx context.Context, custody *atomic.Pointer[dispatcher.PodCustody]) bool {
	return custody != nil && custody.Load() != nil && workerShutdownRequested(ctx)
}

func workerShutdownRequested(ctx context.Context) bool {
	if errors.Is(context.Cause(ctx), worker.ErrWorkerShutdown) {
		return true
	}
	if !activity.IsActivity(ctx) {
		return false
	}
	select {
	case <-activity.GetWorkerStopChannel(ctx):
		return true
	default:
		return false
	}
}

func (a *Activities) dispatchOrAdopt(ctx context.Context, attempt dispatcher.Attempt, input DispatchStageInput) (dispatcher.Report, error) {
	if input.Custody == nil {
		return a.Dispatcher.Dispatch(ctx, attempt, input.Placement.Eligible)
	}
	adopter, ok := a.Dispatcher.(interface {
		AdoptPod(context.Context, dispatcher.Attempt, dispatcher.PodCustody) (dispatcher.Report, error)
	})
	if !ok {
		return dispatcher.Report{}, classifySeamError(errors.New("engine: dispatcher cannot adopt pod custody"))
	}
	return adopter.AdoptPod(ctx, attempt, *input.Custody)
}

func unconfirmedReattachFailure(ctx context.Context, err error, report dispatcher.Report, custody *atomic.Pointer[dispatcher.PodCustody]) (stageActivityResult, error) {
	if errors.Is(err, dispatcher.ErrWorkerHandoff) {
		if held := custody.Load(); held != nil {
			return stageActivityResult{}, dispatchWorkerHandoff(*held)
		}
	}
	return unconfirmedDispatchFailure(ctx, err, report)
}
