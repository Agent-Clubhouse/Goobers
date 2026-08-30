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
	// Charge is the repass-budget transition this outcome produced
	// (gate.RepassBudget.Charge). It is carried so a LATER forced escalation —
	// a duplicate or empty diff — reports the same reason code the runner
	// reports, from the same helper, rather than re-deriving which budget the
	// outcome belonged to.
	Charge gate.RepassCharge
	// Finding lifecycle across repasses (#3843): which of the previous
	// verdict's findings this evaluation resolved, suppressed, reopened, or
	// affirmatively disproved.
	ResolvedFindingIDs   []string
	SuppressedFindingIDs []string
	ReopenedFindingIDs   []string
	DisprovenFindingIDs  []string
	DisprovenFindings    []apiv1.Finding
}

// The walk (engine.go's walk) carries the run's repass accounting through
// evaluateGate as a single gate.RepassBudget, and its four counters are easy
// to mistake for each other because they are all map[string]int and all feed
// a "…Attempt" number into gate.started/gate.evaluated. What each one counts,
// and the fifth counter next door that is NOT part of it:
//
//   - budget.Attempts / budget.InfrastructureAttempts are the gate's
//     consecutive non-pass EVALUATION counts, per class — resolveGateOutcome's
//     ledger, advanced (or reset to 0) only AFTER an evaluator outcome comes
//     back. They exist to recover an interrupted evaluator on replay. Each
//     class resets the other, and a pass resets both. They are incremented
//     exactly once per gate.evaluated and are meaningful for every gate:
//     automated, self-placed agentic, and pod-dispatched agentic alike.
//
//   - budget.RepassAttempts / budget.InfrastructureRepassAttempts are the
//     bounded budgets themselves, per RE-ENTERED TARGET STAGE and cumulative
//     over the run. Policy repasses are bounded by the inherited/per-gate
//     MaxRepasses; infrastructure ones by gate.DefaultMaxInfrastructureRepasses,
//     and an intervening non-infra outcome returns the infrastructure retries
//     the run spent earlier. See gate.RepassBudget for why the two are kept
//     apart, and #3930 for what happened when this side kept only one.
//
//   - gateDispatches (dispatchstage.go's gatePodAttempt) numbers a placed
//     agentic gate's POD dispatches — advanced BEFORE each dispatch, once
//     per pod attempt, including infra retries within a single evaluation
//     that never produced an outcome at all. It is the surrender-plane key
//     and the pod name (D1: one attempt, one pod), so it has to be unique
//     per (run, gate) across retries a per-evaluation number cannot
//     distinguish — a retried evaluation reuses the SAME Attempts value
//     until one finally resolves. Untouched by the self arm, which never
//     creates a pod.
//
// gate.started journals repassAttempt (budget.Attempts[gate]+1, read before
// any counter moves — the POLICY count, exactly as internal/gate's recordStart
// reads e.Attempts) always, and podAttempt (gateDispatches[gate]+1, likewise
// read without mutating) only for a gate about to dispatch to a pod — see
// runJournal.gateStarted.
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
// target's budget.
//
// The charging itself is gate.RepassBudget.Charge — the SAME code
// gate.Evaluator.trackRepass runs for the local runner, not a workflow-side
// re-derivation of it (#3930). This function owns only what is genuinely the
// engine's: resolving the configured branch, and overriding it with the gate's
// escalation target when the charge exhausted its budget.
//
// It used to keep one budget and one bound, and charged infrastructure
// outcomes to both. That is a decision divergence with nothing failing: on the
// implementation lane's `local-gate` (infra: local-ci, a self send-back) an
// engine run escalated a pure infrastructure flake on the FOURTH attempt where
// the runner escalates on the third, and let two content repasses consume the
// budget an infrastructure retry needed.
func resolveGateOutcome(g apiv1.Gate, outcome string, reentry bool, budget *gate.RepassBudget, maxRepasses int) (gateResult, error) {
	target, ok := wf.BranchTarget(g, outcome)
	if !ok {
		return gateResult{}, fmt.Errorf("gate %q: outcome %q has no defined branch (never a silent pass, GT-002)", g.Name, outcome)
	}
	charge := budget.Charge(g, outcome, target, reentry, maxRepasses)
	if charge.Exceeded {
		target = escalationTarget(g)
	}
	return gateResult{
		Gate: g.Name, Outcome: outcome, Target: target, Attempt: charge.Attempt,
		GateAttempt: charge.GateAttempt, RepassTarget: charge.RepassTarget, Escalated: charge.Exceeded,
		Charge: charge,
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
