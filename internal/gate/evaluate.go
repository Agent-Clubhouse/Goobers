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

	// Attempts holds each gate's current consecutive non-pass count, keyed by
	// gate name — the same value Result.Attempt reports after Evaluate. Nil
	// (equivalently, a zero count for every gate) is the correct zero value
	// for a fresh run. A caller resuming an interrupted run seeds this on
	// construction (Evaluator{Attempts: restored, ...}) so the repass budget
	// continues rather than resetting to 0 — e.g. Runner.Resume (#89)
	// reconstructing it from each gate's last gate.evaluated event
	// (Runner["repassAttempt"], recordVerdict in journal.go), the same source
	// state.json itself is always reconstructable from. Evaluate mutates this
	// map in place, so it also serves as the live, inspectable checkpoint
	// source for a caller that wants to persist it after each gate — read
	// Attempts[gateName], not Result.Attempt, if a pass may have reset it
	// since the last read. Nil-safe: Evaluate lazily allocates it on first
	// use. Exported instead of a constructor because every other Evaluator
	// field is already set via struct literal (see internal/runner/run.go).
	Attempts map[string]int
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

	// KNOWN GAP (#263, split out of #112): a crash between the evaluator
	// returning above and this Append succeeding leaves no gate.evaluated
	// event for this attempt at all — unlike a task (stage.started journaled
	// BEFORE dispatch, letting resume.go's interruptedAttempt detect and
	// terminally journal an in-flight crash, #89/#107/#111), a gate has no
	// equivalent pre-dispatch marker, so Resume's gateRepassSeed reconstructs
	// Attempts purely from journaled events and never learns this one
	// happened. A single crash here is a one-off wasted (and, for an agentic
	// gate, side-effecting) duplicate evaluation; a repeated crash-loop
	// hitting this exact window would never actually exhaust the repass
	// budget, defeating the escalation safety net. A real fix needs the same
	// durable pre-dispatch marker pattern tasks have (a new journal event
	// type + resume.go logic) — deferred to #263 rather than folded into this
	// grouped hardening pass; see that issue for the full rationale.
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
	if e.Attempts == nil {
		e.Attempts = make(map[string]int)
	}
	if outcome == OutcomePass {
		e.Attempts[gateName] = 0
		return 0, false
	}
	e.Attempts[gateName]++
	attempt = e.Attempts[gateName]
	budget := e.MaxRepasses
	if budget <= 0 {
		budget = DefaultMaxRepasses
	}
	return attempt, attempt > budget
}
