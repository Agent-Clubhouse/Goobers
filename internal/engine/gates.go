package engine

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	wf "github.com/goobers/goobers/internal/workflow"
)

// gateResult is the engine's resolution of one gate evaluation — the subset
// of internal/gate.Result the workflow decision path needs. The evaluator
// dispatch itself is an activity; this resolution (branch lookup, bounded
// repass, escalation override) is deterministic and runs workflow-side,
// mirroring gate.Evaluator.resolveOutcome/trackRepass exactly. Verdict
// journaling, the identical-diff dedup (#316), the empty-diff fast-fail
// (#415), and the cross-run verdict cache (#523) stay with the local runner
// until the engine has journal-backed diff evidence (#629/#301).
type gateResult struct {
	// Gate is the evaluated gate's name.
	Gate string
	// Outcome is the evaluator outcome.
	Outcome string
	// Target is the branch actually taken — the gate's configured branch for
	// Outcome, unless the repass budget was exhausted, in which case it is
	// the optional escalate control branch or wf.TargetEscalate.
	Target string
	// Attempt is this gate's consecutive non-pass evaluation count (0 on a
	// pass, which resets the budget).
	Attempt int
	// Escalated is true when Target was overridden by the repass budget.
	Escalated bool
}

// maxRepassesFor resolves the run's repass budget: RunInput.MaxRepasses when
// set, else the same gate.DefaultMaxRepasses the local runner defaults to —
// one shared constant, so the budgets cannot drift (#624/#156).
func maxRepassesFor(in RunInput) int {
	if in.MaxRepasses > 0 {
		return in.MaxRepasses
	}
	return gate.DefaultMaxRepasses
}

// resolveGateOutcome resolves an evaluator outcome to the branch taken,
// enforcing the bounded repass budget: a "pass" resets the gate's
// consecutive non-pass count; any other outcome increments it, and exceeding
// maxRepasses escalates through the gate's escalate control branch (or
// wf.TargetEscalate when it has none) instead of following the gate's own
// branch — never a silent loop onward. Ports gate.Evaluator's trackRepass +
// escalation override.
func resolveGateOutcome(g apiv1.Gate, outcome string, attempts map[string]int, maxRepasses int) (gateResult, error) {
	target, ok := wf.BranchTarget(g, outcome)
	if !ok {
		return gateResult{}, fmt.Errorf("gate %q: outcome %q has no defined branch (never a silent pass, GT-002)", g.Name, outcome)
	}
	if outcome == gate.OutcomePass {
		attempts[g.Name] = 0
		return gateResult{Gate: g.Name, Outcome: outcome, Target: target}, nil
	}
	attempts[g.Name]++
	attempt := attempts[g.Name]
	escalated := attempt > maxRepasses
	if escalated {
		target = escalationTarget(g)
	}
	return gateResult{Gate: g.Name, Outcome: outcome, Target: target, Attempt: attempt, Escalated: escalated}, nil
}

// escalationTarget mirrors internal/gate's escalationTarget: forced
// escalation routes through the gate's optional escalate control branch,
// terminating at @escalate when it has none.
func escalationTarget(g apiv1.Gate) string {
	if target, ok := wf.BranchTarget(g, wf.BranchEscalate); ok {
		return target
	}
	return wf.TargetEscalate
}

// evaluateWithInfraRetry mirrors internal/gate.Evaluator.evaluateWithRetry
// (#765): a gate's declared evaluator retry bound applies to transient
// (infrastructure-classed) evaluator failures only. A policy-classed error —
// a misconfiguration, a business failure, anything unmarked — returns
// immediately, and exhausting the bound returns the last error: both fail
// the run exactly as a gate with no retry block would. Each attempt's
// dispatch runs under its own start-to-close window, so a retry gets a fresh
// timeout.
func evaluateWithInfraRetry(ctx workflow.Context, g apiv1.Gate, rec *runJournal, call func(workflow.Context) error) error {
	maxAttempts, backoff := evaluatorRetryBounds(gateEvaluatorRetry(g))
	for attempt := 1; ; attempt++ {
		err := call(ctx)
		if err == nil {
			return nil
		}
		if temporal.IsCanceledError(err) || ctx.Err() != nil {
			return err
		}
		class, cerr := attemptFailureClass(err)
		if cerr != nil {
			return cerr
		}
		if class != journal.AttemptInfra {
			return err
		}
		// Every transient evaluator failure is journaled (#765's
		// recordEvaluatorRetry parity), including the one that exhausts the
		// bound — the local evaluator records before it gives up too.
		rec.evaluatorRetry(ctx, g.Name, attempt, err)
		if attempt >= maxAttempts {
			// Bound exhausted — fail the run, never a silent infinite retry.
			return err
		}
		if backoff > 0 {
			if serr := workflow.Sleep(ctx, backoff); serr != nil {
				return serr
			}
		}
	}
}

// gateEvaluatorRetry reads the gate's declared evaluator retry policy off its
// evaluator sub-config — the same DSL fields internal/gate's gateRetryPolicy
// reads (#151/#765). nil when the gate declares no retry.
func gateEvaluatorRetry(g apiv1.Gate) *apiv1.RetryPolicy {
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

// evaluatorRetryBounds mirrors internal/gate's retryBounds: a nil policy —
// or MaxAttempts <= 1 — means a single attempt, so only a gate that opts in
// via retry: ever retries.
func evaluatorRetryBounds(policy *apiv1.RetryPolicy) (maxAttempts int, backoff time.Duration) {
	maxAttempts = 1
	if policy != nil && policy.MaxAttempts > 1 {
		maxAttempts = int(policy.MaxAttempts)
		backoff = time.Duration(policy.BackoffSeconds) * time.Second
	}
	return maxAttempts, backoff
}
