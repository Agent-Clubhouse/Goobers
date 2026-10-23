package runtimewiring

import (
	"context"
	"errors"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
)

// ExecutionFenceStart starts ownership observation for an invocation and returns its stop function.
type ExecutionFenceStart func(context.Context, apiv1.InvocationEnvelope) (context.Context, context.CancelFunc, error)

// ClaimFencedDeterministic checks claim ownership around a deterministic stage.
type ClaimFencedDeterministic struct {
	invoke.Deterministic
	Start ExecutionFenceStart
}

// Run runs a stage through the configured execution safeguards.
func (e ClaimFencedDeterministic) Run(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	ctx, stop, err := e.Start(ctx, env)
	defer stop()
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	result, err := e.Deterministic.Run(ctx, env, run)
	return result, errors.Join(err, context.Cause(ctx))
}

// ClaimFencedGoober checks claim ownership around goober invocation and review.
type ClaimFencedGoober struct {
	invoke.Goober
	Start ExecutionFenceStart
}

// HasAssetBundle preserves the runner's asset protection contract across the wrapper.
func (e ClaimFencedGoober) HasAssetBundle() bool {
	assets, ok := e.Goober.(interface{ HasAssetBundle() bool })
	return ok && assets.HasAssetBundle()
}

// Invoke invokes a goober while observing claim ownership.
func (e ClaimFencedGoober) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	ctx, stop, err := e.Start(ctx, env)
	defer stop()
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	result, err := e.Goober.Invoke(ctx, env)
	return result, errors.Join(err, context.Cause(ctx))
}

// Review requests a review while observing claim ownership.
func (e ClaimFencedGoober) Review(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	ctx, stop, err := e.Start(ctx, env)
	defer stop()
	if err != nil {
		return apiv1.Verdict{}, err
	}
	result, err := e.Goober.Review(ctx, env)
	return result, errors.Join(err, context.Cause(ctx))
}
