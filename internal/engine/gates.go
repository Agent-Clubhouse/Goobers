package engine

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runcontrol"
	wf "github.com/goobers/goobers/internal/workflow"
)

// gateResult is the engine's resolution of one gate evaluation — the subset
// of internal/gate.Result the workflow decision path needs. The evaluator
// dispatch itself is an activity; this resolution (branch lookup, bounded
// repass, escalation override) is deterministic and runs workflow-side,
// mirroring gate.Evaluator.resolveOutcome/trackRepass exactly. Verdict
// journaling (runJournal.gateEvaluated) and the #412 verdict ContextPointer
// are engine-side. The diff-evidence features — identical-diff dedup (#316),
// empty-diff fast-fail (#415), cross-run verdict cache (#523) — landed with
// #3882 and are carried on the fields below.
type gateResult struct {
	// Gate is the evaluated gate's name.
	Gate string
	// Outcome is the evaluator outcome.
	Outcome string
	// Target is the branch actually taken — the gate's configured branch for
	// Outcome, unless the repass budget was exhausted, in which case it is
	// the optional escalate control branch or wf.TargetEscalate.
	Target string
	// Attempt is the target stage's cumulative re-entry count, or 0 when this
	// branch does not re-enter a completed stage.
	Attempt int
	// GateAttempt is this gate's consecutive non-pass evaluation count.
	GateAttempt int
	// RepassTarget is the configured branch target charged by Attempt.
	RepassTarget string
	// Escalated is true when Target was overridden by the repass budget.
	Escalated bool
	// DiffDigest is the content digest of the subject diff this gate was
	// shown (#3384). Empty for an automated or human gate, and for an agentic
	// gate whose workspace could not report one.
	DiffDigest string
	// DuplicateDiff records that the subject produced a diff byte-identical to
	// the one the previous repass produced (#316), so the reviewer was NOT
	// invoked and the verdict was synthesized.
	DuplicateDiff bool
	// CacheHit records that the subject carried a verdict forward and the
	// reviewer was NOT invoked (#523).
	CacheHit bool
	// RepassCause is why the subject stage was re-entered (#3375). Nil on a
	// first pass.
	RepassCause *gate.RepassCause
	// Reason is the synthesized outcome's own explanation, journaled so an
	// operator can tell an empty-diff fast-fail from a reviewer's fail.
	Reason string
	// Finding lifecycle across repasses (#3843): which of the previous
	// verdict's findings this evaluation resolved, suppressed, reopened, or
	// affirmatively disproved.
	ResolvedFindingIDs   []string
	SuppressedFindingIDs []string
	ReopenedFindingIDs   []string
	DisprovenFindingIDs  []string
	DisprovenFindings    []apiv1.Finding
}

// The walk (engine.go's walk) carries two per-gate map[string]int counters
// through evaluateGate, and they are easy to mistake for each other because
// both are keyed by gate name and both feed a "…Attempt" number into
// gate.started/gate.evaluated. They count two different things:
//
//   - gateAttempts is the gate's consecutive non-pass EVALUATION count —
//     resolveGateOutcome's ledger, advanced (or reset to 0 on a pass) only
//     AFTER an evaluator outcome comes back. It exists to recover an
//     interrupted evaluator on replay and to charge the repass budget
//     (repassAttempts, per re-entered target). It is incremented exactly
//     once per gate.evaluated and is meaningful for every gate: automated,
//     self-placed agentic, and pod-dispatched agentic alike.
//
//   - gateDispatches (dispatchstage.go's gatePodAttempt) numbers a placed
//     agentic gate's POD dispatches — advanced BEFORE each dispatch, once
//     per pod attempt, including infra retries within a single evaluation
//     that never produced an outcome at all. It is the surrender-plane key
//     and the pod name (D1: one attempt, one pod), so it has to be unique
//     per (run, gate) across retries a gateAttempts-keyed number cannot
//     distinguish — a retried evaluation reuses the SAME gateAttempts value
//     until one finally resolves. Untouched by the self arm, which never
//     creates a pod.
//
// gate.started journals repassAttempt (gateAttempts[gate]+1, read before
// either counter moves) always, and podAttempt (gateDispatches[gate]+1,
// likewise read without mutating) only for a gate about to dispatch to a
// pod — see runJournal.gateStarted.
//
// maxRepassesFor resolves the inherited run budget, retaining the legacy
// RunInput.MaxRepasses fallback for persisted inputs created before RunControls.
func maxRepassesFor(in RunInput) int {
	if in.RunControls.MaxRepasses > 0 {
		return int(in.RunControls.MaxRepasses)
	}
	if in.MaxRepasses > 0 {
		return in.MaxRepasses
	}
	return gate.DefaultMaxRepasses
}

// resolveGateOutcome resolves an evaluator outcome to the branch taken and
// charges every branch that re-enters an already-completed stage to that
// target's shared budget. Ports gate.Evaluator's trackRepass and escalation
// override.
func resolveGateOutcome(g apiv1.Gate, outcome string, reentry bool, gateAttempts, repassAttempts map[string]int, maxRepasses int) (gateResult, error) {
	target, ok := wf.BranchTarget(g, outcome)
	if !ok {
		return gateResult{}, fmt.Errorf("gate %q: outcome %q has no defined branch (never a silent pass, GT-002)", g.Name, outcome)
	}
	if outcome == gate.OutcomePass {
		gateAttempts[g.Name] = 0
	} else {
		gateAttempts[g.Name]++
	}
	gateAttempt := gateAttempts[g.Name]
	if !reentry {
		return gateResult{Gate: g.Name, Outcome: outcome, Target: target, GateAttempt: gateAttempt}, nil
	}
	repassAttempts[target]++
	attempt := repassAttempts[target]
	escalated := attempt > runcontrol.MaxRepassesForGate(g, maxRepasses)
	if escalated {
		target = escalationTarget(g)
	}
	return gateResult{
		Gate: g.Name, Outcome: outcome, Target: target, Attempt: attempt,
		GateAttempt: gateAttempt, RepassTarget: wfTarget(g, outcome), Escalated: escalated,
	}, nil
}

func wfTarget(g apiv1.Gate, outcome string) string {
	target, _ := wf.BranchTarget(g, outcome)
	return target
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
		class, cerr := ClassifyDispatchFailure(err)
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
