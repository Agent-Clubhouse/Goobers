package gate

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/runcontrol"
	wf "github.com/goobers/goobers/internal/workflow"
)

// RepassBudget is the repass accounting a single run charges every gate
// evaluation against, and the ONE place the arithmetic behind it lives
// (#3930).
//
// A gate outcome that re-enters an already-completed stage is a repass, and a
// repass is bounded. There are two budgets, not one, and keeping them apart is
// the whole point of the type:
//
//   - POLICY repasses are the reviewer's/checker's judgement that the WORK is
//     not done — a needs-changes verdict, a failing check. They are bounded by
//     the run's inherited MaxRepasses, overridable per gate
//     (runcontrol.MaxRepassesForGate, default 3).
//   - INFRASTRUCTURE repasses (OutcomeInfra) are the producer's own claim that
//     the MACHINERY failed rather than the work — a lost runner, a worktree
//     that would not provision. They are bounded by
//     DefaultMaxInfrastructureRepasses (2) and charged to their own counters,
//     so an environment flake cannot spend the budget a code-review loop needs.
//
// Each budget is kept twice, at two different keys, because they answer two
// different questions:
//
//   - Attempts / InfrastructureAttempts are PER GATE. They are the gate's
//     consecutive non-pass evaluation count in that class — what recovers an
//     interrupted evaluation after a crash and what a gate.started marker
//     numbers. They CROSS-RESET: an infra outcome zeroes the gate's policy
//     count and vice versa, because "three consecutive review failures" stops
//     being true the moment the machinery, not the work, is what failed.
//   - RepassAttempts / InfrastructureRepassAttempts are PER TARGET STAGE.
//     They are the bounded budget itself, cumulative over the whole run for
//     each re-entered stage, because two gates sending the same stage back
//     four times is the same non-convergence as one gate doing it four times.
//     The infra one is cross-reset too, keyed by the gate's declared infra
//     branch: an intervening non-infra outcome means the run made real
//     progress, and the infrastructure retry budget it consumed earlier is
//     returned rather than held against it for the rest of the run.
//
// # Why this is a type and not a method on Evaluator
//
// Both drivers charge it. internal/gate.Evaluator.trackRepass does it for the
// local runner; internal/engine's resolveGateOutcome does it workflow-side for
// the Temporal engine, where the evaluator dispatch is an activity and the
// decision must stay deterministic. The engine's port re-derived the
// arithmetic by hand and collapsed it: infrastructure outcomes were charged to
// the POLICY counter, at the POLICY bound, and were never cross-reset (#3930).
// Nothing failed — every parity row that reaches a retry arm uses a
// status-equals gate, which cannot produce OutcomeInfra at all — so the same
// definition over the same failures escalated at a different point on the two
// runners.
//
// So the arithmetic lives here, once, and both drivers call Charge. It follows
// the shared-constant pattern #624 established for RetryFailureClass and
// BuildLearningEpisode, for the same reason: a second hand-written copy is how
// this diverged.
//
// # Determinism and persistence
//
// Charge is a pure function of (budget state, gate, outcome, target, reentry,
// inherited budget): no clock, no randomness, no I/O, no map iteration. The
// engine holds a RepassBudget as ordinary workflow state and a replay
// reconstructs it from the same deterministic outcome sequence; the runner
// holds it inside the Evaluator and a resume re-seeds it from the journaled
// repassTarget/repassAttempt/gateAttempt annotations. Every map is
// zero-value-usable: a nil map reads as 0 for every key, which is exactly what
// a history recorded before the counters existed must mean.
type RepassBudget struct {
	// Attempts is each gate's consecutive non-pass POLICY evaluation count.
	Attempts map[string]int
	// InfrastructureAttempts is each gate's consecutive OutcomeInfra
	// evaluation count.
	InfrastructureAttempts map[string]int
	// RepassAttempts is the cumulative POLICY repass count per re-entered
	// target stage — the budget MaxRepasses bounds.
	RepassAttempts map[string]int
	// InfrastructureRepassAttempts is the cumulative infrastructure repass
	// count per re-entered target stage — the budget
	// DefaultMaxInfrastructureRepasses bounds.
	InfrastructureRepassAttempts map[string]int
}

// RepassCharge is what one Charge did: the numbers the caller journals and the
// single decision (Exceeded) it routes on.
type RepassCharge struct {
	// Attempt is the target stage's cumulative re-entry count in this
	// outcome's class, including this evaluation. 0 when the branch does not
	// re-enter a completed stage.
	Attempt int
	// GateAttempt is the gate's consecutive non-pass evaluation count in this
	// outcome's class. 0 on a pass.
	GateAttempt int
	// RepassTarget is the configured branch target charged by Attempt. Empty
	// when nothing was charged. It survives an escalation override, so a
	// resume can re-seed the counter the exhausted evaluation belonged to.
	RepassTarget string
	// Infrastructure records which budget was charged, so a caller can pick
	// the escalation reason code (ReasonInfrastructureBudgetExhausted vs
	// ReasonRepassBudgetExhausted) from the charge rather than re-deriving it
	// from the outcome string.
	Infrastructure bool
	// Bound is the budget Attempt was compared against — the per-gate/
	// inherited policy budget, or DefaultMaxInfrastructureRepasses.
	Bound int
	// Exceeded is true when Attempt passed Bound: the caller must override the
	// gate's configured branch with its escalation target.
	Exceeded bool
}

// Charge applies one gate outcome to the budget and reports what it cost.
//
// target is the gate's CONFIGURED branch for outcome (never an escalation
// override — the override is the caller's response to Exceeded). reentry is
// whether that target is a stage that has already completed in this run; the
// two drivers answer it differently (the runner from its visited-stage set,
// the engine from its upstream results map) and neither answer belongs here.
// maxRepasses is the run's INHERITED policy budget; the per-gate leaf override
// is applied here so both drivers resolve it identically.
//
// The counter mutations happen before the reentry check, and deliberately: a
// gate's per-gate class counters advance on every evaluation, including the
// ones whose branch routes forward and charges no budget at all.
func (b *RepassBudget) Charge(g apiv1.Gate, outcome, target string, reentry bool, maxRepasses int) RepassCharge {
	infrastructure := outcome == OutcomeInfra
	if b.Attempts == nil {
		b.Attempts = make(map[string]int)
	}
	if b.InfrastructureAttempts == nil {
		b.InfrastructureAttempts = make(map[string]int)
	}
	// (c) The cross-reset on the per-TARGET infrastructure budget: any outcome
	// that is not itself infrastructure means the run got past the machinery
	// fault, so the retries it spent are returned. Keyed by the gate's own
	// declared infra branch rather than by the target just taken — the
	// infrastructure target is usually a DIFFERENT stage from the one a
	// content failure sends back to (implementation's local-gate: infra
	// re-runs local-ci, fail re-implements).
	if !infrastructure && b.InfrastructureRepassAttempts != nil {
		if infrastructureTarget, ok := wf.BranchTarget(g, OutcomeInfra); ok {
			b.InfrastructureRepassAttempts[infrastructureTarget] = 0
		}
	}
	charge := RepassCharge{
		Infrastructure: infrastructure,
		Bound:          runcontrol.MaxRepassesForGate(g, maxRepasses),
	}
	if infrastructure {
		charge.Bound = DefaultMaxInfrastructureRepasses
	}
	// The per-GATE cross-resets: a pass clears both classes, and each non-pass
	// class clears the other.
	switch {
	case outcome == OutcomePass:
		b.Attempts[g.Name] = 0
		b.InfrastructureAttempts[g.Name] = 0
	case infrastructure:
		b.Attempts[g.Name] = 0
		b.InfrastructureAttempts[g.Name]++
		charge.GateAttempt = b.InfrastructureAttempts[g.Name]
	default:
		b.InfrastructureAttempts[g.Name] = 0
		b.Attempts[g.Name]++
		charge.GateAttempt = b.Attempts[g.Name]
	}
	if !reentry {
		return charge
	}
	// (a)/(b) The two per-target budgets: separate counter, separate bound.
	if infrastructure {
		if b.InfrastructureRepassAttempts == nil {
			b.InfrastructureRepassAttempts = make(map[string]int)
		}
		b.InfrastructureRepassAttempts[target]++
		charge.Attempt = b.InfrastructureRepassAttempts[target]
	} else {
		if b.RepassAttempts == nil {
			b.RepassAttempts = make(map[string]int)
		}
		b.RepassAttempts[target]++
		charge.Attempt = b.RepassAttempts[target]
	}
	charge.RepassTarget = target
	charge.Exceeded = charge.Attempt > charge.Bound
	return charge
}

// EscalationReason is the reason code an exhausted budget escalates under.
// Both drivers journal it, and which one depends on WHICH budget ran out —
// telemetry distinguishes policy repass churn from an exhausted infrastructure
// retry budget.
func (c RepassCharge) EscalationReason() string {
	if c.Infrastructure {
		return ReasonInfrastructureBudgetExhausted
	}
	return ReasonRepassBudgetExhausted
}
