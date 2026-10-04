package runner

import (
	"context"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

// GatePreparationRetryFloor is the minimum wait before re-attempting a gate's
// workspace preparation after a retryable provisioning failure (#5446). The
// common retryable case is a run branch still checked out by a sibling run
// that is about to finish; an immediate retry against that occupant is
// all but guaranteed to fail the same way, and the shipped review gates
// declare maxAttempts without backoffSeconds. A declared backoff longer than
// the floor still wins. It applies only between preparation attempts the
// gate's own retry budget already allows — never adds an attempt.
const GatePreparationRetryFloor = time.Minute

func gatePreparationRetryPolicy(g apiv1.Gate) *apiv1.RetryPolicy {
	switch g.Evaluator {
	case apiv1.EvaluatorAutomated:
		if g.Automated != nil {
			return g.Automated.Retry
		}
	case apiv1.EvaluatorAgentic:
		if g.Agentic != nil {
			return g.Agentic.Retry
		}
	}
	return nil
}

func gatePreparationRetryBounds(policy *apiv1.RetryPolicy) (int, time.Duration) {
	if policy == nil || policy.MaxAttempts <= 1 {
		return 1, 0
	}
	return int(policy.MaxAttempts), time.Duration(policy.BackoffSeconds) * time.Second
}

func gateWithConsumedPreparationAttempts(g apiv1.Gate, consumed int) apiv1.Gate {
	if consumed <= 0 {
		return g
	}
	policy := gatePreparationRetryPolicy(g)
	if policy == nil || policy.MaxAttempts <= 1 {
		return g
	}
	remaining := policy.MaxAttempts - int32(consumed)
	if remaining < 1 {
		remaining = 1
	}
	nextPolicy := *policy
	nextPolicy.MaxAttempts = remaining
	switch g.Evaluator {
	case apiv1.EvaluatorAutomated:
		if g.Automated != nil {
			automated := *g.Automated
			automated.Retry = &nextPolicy
			g.Automated = &automated
		}
	case apiv1.EvaluatorAgentic:
		if g.Agentic != nil {
			agentic := *g.Agentic
			agentic.Retry = &nextPolicy
			g.Agentic = &agentic
		}
	}
	return g
}

func (r *Runner) buildGateEnvelopeWithRetry(ctx context.Context, jr journalAppender, in StartInput, g apiv1.Gate, gateCaps []string, gateLimits apiv1.Limits, upstream []apiv1.ContextPointer, workspaceBranch string) (apiv1.InvocationEnvelope, *stageWorkspace, int, error) {
	maxAttempts, backoff := gatePreparationRetryBounds(gatePreparationRetryPolicy(g))
	backoff = max(backoff, r.gatePrepRetryFloor)
	for attempt := 1; ; attempt++ {
		env, workspace, err := r.buildEnvelope(ctx, in, g.Name, "gate: "+g.Name, nil, gateCaps, gateLimits, upstream, gateWorkspaceMode(g), false, workspaceBranch)
		if err == nil {
			return env, workspace, attempt - 1, nil
		}
		if !worktree.IsRetryableProvisionError(err) || attempt >= maxAttempts {
			return apiv1.InvocationEnvelope{}, nil, attempt - 1, err
		}
		if backoff > 0 {
			if waitErr := waitForRetry(ctx, ctx, jr, g.Name, attempt, journal.AttemptInfra, backoff); waitErr != nil {
				return apiv1.InvocationEnvelope{}, nil, attempt - 1, waitErr
			}
		}
	}
}
