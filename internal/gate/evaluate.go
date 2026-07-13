package gate

import (
	"context"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	wf "github.com/goobers/goobers/internal/workflow"
)

// DefaultMaxRepasses is the default bounded-repass budget: a gate may
// evaluate to a non-"pass" outcome this many times before Evaluator overrides
// its own configured branch and routes to workflow.TargetEscalate instead —
// "bounded repass loops (loop budget journaled and enforced)" (issue #20).
// There is no per-gate budget field in the workflow DSL yet (api/v1alpha1
// Gate/AutomatedGate/AgenticGate); this is a run-wide default, overridable via
// Evaluator.MaxRepasses.
const DefaultMaxRepasses = 3

// Result is the outcome of one gate evaluation.
type Result struct {
	// Gate is the evaluated gate's name.
	Gate string
	// Outcome is the raw evaluator outcome (a check's "pass"/"fail", or an
	// agentic Verdict's Decision string).
	Outcome string
	// Target is the branch actually taken — the gate's configured branch for
	// Outcome, unless the repass budget was exhausted, in which case it is
	// workflow.TargetEscalate.
	Target string
	// Attempt is this gate's consecutive non-pass evaluation count,
	// including this one when Outcome != OutcomePass (0 when Outcome ==
	// OutcomePass, since a pass resets the budget).
	Attempt int
	// Escalated is true when Target was overridden by the repass budget
	// rather than resolved from the gate's own Branches.
	Escalated bool
	// Verdict is the full agentic-gate verdict (decision, rationale,
	// evidence, findings). nil for automated gates.
	Verdict *apiv1.Verdict
}

// Evaluator dispatches a gate to its configured evaluator (automated or
// agentic — human gates are V1, GT-003), resolves the outcome to a branch via
// the compiled machine, enforces the bounded-repass budget, and journals the
// verdict. It is safe for reuse across every gate evaluation within a single
// run; it is NOT safe for concurrent use (a run advances one state at a time)
// and MUST NOT be shared across runs (repass counts are per-run state).
type Evaluator struct {
	// Automated evaluates automated gates. Required if any gate in the
	// workflow is evaluator=automated.
	Automated invoke.Automated
	// Reviewer evaluates agentic gates. Required if any gate in the workflow
	// is evaluator=agentic.
	Reviewer *ReviewerEvaluator
	// Journal records gate verdicts. Optional — nil disables journaling
	// (e.g. in unit tests that only care about branch resolution).
	Journal Journal
	// MaxRepasses overrides DefaultMaxRepasses when non-zero.
	MaxRepasses int

	attempts map[string]int
}

// Evaluate runs gate g's evaluator against env (already built by the caller,
// including — for automated gates — the flattened Inputs convention
// documented in automated.go), attaches subject's artifacts as evidence for
// agentic gates, resolves the branch via the compiled machine's Branches
// (workflow.Compile already validated every branch target resolves to a real
// state or a reserved terminal), enforces the repass budget, and journals the
// result.
func (e *Evaluator) Evaluate(ctx context.Context, g apiv1.Gate, env apiv1.InvocationEnvelope, subjectStage string, subject apiv1.ResultEnvelope) (Result, error) {
	var outcome string
	var verdict *apiv1.Verdict

	switch g.Evaluator {
	case apiv1.EvaluatorAutomated:
		if e.Automated == nil {
			return Result{}, fmt.Errorf("gate %q: automated evaluator not configured", g.Name)
		}
		conf := apiv1.AutomatedGate{}
		if g.Automated != nil {
			conf = *g.Automated
		}
		out, err := e.Automated.Evaluate(ctx, conf, env)
		if err != nil {
			return Result{}, fmt.Errorf("gate %q: evaluate automated: %w", g.Name, err)
		}
		outcome = out

	case apiv1.EvaluatorAgentic:
		if e.Reviewer == nil {
			return Result{}, fmt.Errorf("gate %q: agentic reviewer not configured", g.Name)
		}
		v, err := e.Reviewer.Review(ctx, env, subjectStage, subject)
		if err != nil {
			return Result{}, fmt.Errorf("gate %q: reviewer evaluation: %w", g.Name, err)
		}
		verdict = &v
		outcome = string(v.Decision)

	case apiv1.EvaluatorHuman:
		return Result{}, fmt.Errorf("gate %q: human evaluator is not supported at V0 (GT-003, ships V1)", g.Name)

	default:
		return Result{}, fmt.Errorf("gate %q: unknown evaluator %q", g.Name, g.Evaluator)
	}

	target, ok := wf.BranchTarget(g, outcome)
	if !ok {
		return Result{}, fmt.Errorf("gate %q: outcome %q has no defined branch (never a silent pass, GT-002)", g.Name, outcome)
	}

	attempt, escalated := e.trackRepass(g.Name, outcome)
	if escalated {
		target = wf.TargetEscalate
	}

	r := Result{Gate: g.Name, Outcome: outcome, Target: target, Attempt: attempt, Escalated: escalated, Verdict: verdict}
	if err := recordVerdict(e.Journal, r); err != nil {
		return Result{}, fmt.Errorf("gate %q: journal verdict: %w", g.Name, err)
	}
	return r, nil
}

// trackRepass updates gate g's consecutive non-pass counter: a "pass" outcome
// resets it to 0; any other outcome increments it. It returns the post-update
// count and whether that count exceeds the repass budget (in which case the
// caller must escalate instead of following the gate's own branch).
func (e *Evaluator) trackRepass(gateName, outcome string) (attempt int, exceeded bool) {
	if e.attempts == nil {
		e.attempts = make(map[string]int)
	}
	if outcome == OutcomePass {
		e.attempts[gateName] = 0
		return 0, false
	}
	e.attempts[gateName]++
	attempt = e.attempts[gateName]
	budget := e.MaxRepasses
	if budget <= 0 {
		budget = DefaultMaxRepasses
	}
	return attempt, attempt > budget
}
