package engine

import (
	"context"
	"encoding/json"

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
	return a.InvokeGoober(
		ctx, env, workspaceBranch, workspaceDelta, workspace, onTimeout)
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
	return a.InvokeGooberExact(
		ctx, env, workspaceBranch, workspaceDelta,
		workspace, onTimeout, exact)
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
	return a.RunDeterministic(
		ctx, env, run, workspaceBranch, workspaceDelta)
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
	return a.RunDeterministicExact(
		ctx, env, run, workspaceBranch, workspaceDelta, exact)
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
	return a.ReviewGoober(
		ctx, env, workspaceBranch, workspaceDelta,
		workspace, priorDiffDigest, requireDiff)
}

// EvaluateAutomatedCancelSafe heartbeats while an automated gate runs.
func (a *Activities) EvaluateAutomatedCancelSafe(
	ctx context.Context,
	gate apiv1.AutomatedGate,
	env apiv1.InvocationEnvelope,
) (string, error) {
	stop := heartbeatDispatch(ctx)
	defer stop()
	return a.EvaluateAutomated(ctx, gate, env)
}
