package engine

import (
	"context"

	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/launchreceipt"
)

// New activity names preserve all legacy positional payloads on replay. An
// outstanding legacy launch reaches the production executor without authority
// and fails closed; a completed legacy activity still replays unchanged.
const (
	actInvokeGooberPrepared     = "InvokeGooberPrepared"
	actReviewGooberPrepared     = "ReviewGooberPrepared"
	actRunDeterministicPrepared = "RunDeterministicPrepared"
)

func executeLocalTask(ctx workflow.Context, rec *runJournal, task apiv1.Task, env apiv1.InvocationEnvelope, run *apiv1.DeterministicRun, branch, delta string) workflow.Future {
	binding := rec.localLaunchBinding(ctx, task.Name, false)
	if run == nil {
		if binding == nil {
			return workflow.ExecuteActivity(ctx, ActInvokeGoober, env, branch, delta, task.EffectiveWorkspace(), task.OnTimeout)
		}
		return workflow.ExecuteActivity(ctx, actInvokeGooberPrepared, env, branch, delta, task.EffectiveWorkspace(), task.OnTimeout, *binding)
	}
	if binding == nil {
		return workflow.ExecuteActivity(ctx, ActRunDeterministic, env, *run, branch, delta)
	}
	return workflow.ExecuteActivity(ctx, actRunDeterministicPrepared, env, *run, branch, delta, *binding)
}

func executeLocalReviewer(ctx workflow.Context, rec *runJournal, gate apiv1.Gate, env apiv1.InvocationEnvelope, branch, delta, priorDiff string, subjectAgentic bool) workflow.Future {
	binding := rec.localLaunchBinding(ctx, gate.Name, true)
	if binding == nil {
		return workflow.ExecuteActivity(ctx, ActReviewGoober, env, branch, delta, gate.EffectiveWorkspace(), priorDiff, subjectAgentic)
	}
	return workflow.ExecuteActivity(ctx, actReviewGooberPrepared, env, branch, delta, gate.EffectiveWorkspace(), priorDiff, subjectAgentic, *binding)
}

// InvokeGooberPrepared receives authority only from a versioned controller
// workflow payload, then keeps it out of the model's invocation envelope.
func (a *Activities) InvokeGooberPrepared(ctx context.Context, env apiv1.InvocationEnvelope, branch, delta string, workspace apiv1.WorkspaceMode, onTimeout string, binding launchreceipt.Binding) (stageActivityResult, error) {
	ctx = launchreceipt.WithBinding(ctx, binding)
	if _, err := launchreceipt.ForInvocation(ctx, env, false); err != nil {
		return stageActivityResult{}, err
	}
	return a.InvokeGoober(ctx, env, branch, delta, workspace, onTimeout)
}

// ReviewGooberPrepared binds each reviewer retry independently.
func (a *Activities) ReviewGooberPrepared(ctx context.Context, env apiv1.InvocationEnvelope, branch, delta string, workspace apiv1.WorkspaceMode, priorDiff string, subjectAgentic bool, binding launchreceipt.Binding) (GateReviewResult, error) {
	ctx = launchreceipt.WithBinding(ctx, binding)
	if _, err := launchreceipt.ForInvocation(ctx, env, true); err != nil {
		return GateReviewResult{}, err
	}
	return a.ReviewGoober(ctx, env, branch, delta, workspace, priorDiff, subjectAgentic)
}

// RunDeterministicPrepared covers shell and in-process builtin execution.
func (a *Activities) RunDeterministicPrepared(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun, branch, delta string, binding launchreceipt.Binding) (stageActivityResult, error) {
	ctx = launchreceipt.WithBinding(ctx, binding)
	if _, err := launchreceipt.ForInvocation(ctx, env, false); err != nil {
		return stageActivityResult{}, err
	}
	return a.RunDeterministic(ctx, env, run, branch, delta)
}
