package engine

import (
	"context"
	"encoding/json"
	"errors"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

const (
	actInvokeGooberCancelSafe          = "InvokeGooberCancelSafe"
	actInvokeGooberExactCancelSafe     = "InvokeGooberExactCancelSafe"
	actRunDeterministicCancelSafe      = "RunDeterministicCancelSafe"
	actRunDeterministicExactCancelSafe = "RunDeterministicExactCancelSafe"
	actReviewGooberCancelSafe          = "ReviewGooberCancelSafe"
	actEvaluateAutomatedCancelSafe     = "EvaluateAutomatedCancelSafe"
)

// InvokeGooberCancelSafe heartbeats while the ordinary agentic activity runs.
func (a *Activities) InvokeGooberCancelSafe(
	ctx context.Context,
	env apiv1.InvocationEnvelope,
	workspaceBranch string,
	workspaceDelta string,
	workspace apiv1.WorkspaceMode,
	onTimeout string,
) (stageActivityResult, error) {
	stop := heartbeatDispatch(ctx)
	defer stop()
	result, err := a.InvokeGoober(
		ctx, env, workspaceBranch, workspaceDelta, workspace, onTimeout)
	return result, cancelSafeActivityError(ctx, err)
}

// InvokeGooberExactCancelSafe heartbeats an exact-input agentic activity.
func (a *Activities) InvokeGooberExactCancelSafe(
	ctx context.Context,
	env apiv1.InvocationEnvelope,
	workspaceBranch string,
	workspaceDelta string,
	workspace apiv1.WorkspaceMode,
	onTimeout string,
	exact map[string]json.RawMessage,
) (stageActivityResult, error) {
	stop := heartbeatDispatch(ctx)
	defer stop()
	result, err := a.InvokeGooberExact(
		ctx, env, workspaceBranch, workspaceDelta,
		workspace, onTimeout, exact)
	return result, cancelSafeActivityError(ctx, err)
}

// RunDeterministicCancelSafe heartbeats while deterministic work runs.
func (a *Activities) RunDeterministicCancelSafe(
	ctx context.Context,
	env apiv1.InvocationEnvelope,
	run apiv1.DeterministicRun,
	workspaceBranch string,
	workspaceDelta string,
) (stageActivityResult, error) {
	stop := heartbeatDispatch(ctx)
	defer stop()
	result, err := a.RunDeterministic(
		ctx, env, run, workspaceBranch, workspaceDelta)
	return result, cancelSafeActivityError(ctx, err)
}

// RunDeterministicExactCancelSafe heartbeats exact-input deterministic work.
func (a *Activities) RunDeterministicExactCancelSafe(
	ctx context.Context,
	env apiv1.InvocationEnvelope,
	run apiv1.DeterministicRun,
	workspaceBranch string,
	workspaceDelta string,
	exact map[string]json.RawMessage,
) (stageActivityResult, error) {
	stop := heartbeatDispatch(ctx)
	defer stop()
	result, err := a.RunDeterministicExact(
		ctx, env, run, workspaceBranch, workspaceDelta, exact)
	return result, cancelSafeActivityError(ctx, err)
}

// ReviewGooberCancelSafe heartbeats while an agentic gate runs.
func (a *Activities) ReviewGooberCancelSafe(
	ctx context.Context,
	env apiv1.InvocationEnvelope,
	workspaceBranch string,
	workspaceDelta string,
	workspace apiv1.WorkspaceMode,
	priorDiffDigest string,
	requireDiff bool,
) (GateReviewResult, error) {
	stop := heartbeatDispatch(ctx)
	defer stop()
	result, err := a.ReviewGoober(
		ctx, env, workspaceBranch, workspaceDelta,
		workspace, priorDiffDigest, requireDiff)
	return result, cancelSafeActivityError(ctx, err)
}

// EvaluateAutomatedCancelSafe heartbeats while an automated gate runs.
func (a *Activities) EvaluateAutomatedCancelSafe(
	ctx context.Context,
	gate apiv1.AutomatedGate,
	env apiv1.InvocationEnvelope,
) (string, error) {
	stop := heartbeatDispatch(ctx)
	defer stop()
	result, err := a.EvaluateAutomated(ctx, gate, env)
	return result, cancelSafeActivityError(ctx, err)
}

func cancelSafeActivityError(ctx context.Context, err error) error {
	if err != nil && errors.Is(ctx.Err(), context.Canceled) &&
		!errors.Is(context.Cause(ctx), worker.ErrWorkerShutdown) {
		return temporal.NewCanceledError()
	}
	return err
}
