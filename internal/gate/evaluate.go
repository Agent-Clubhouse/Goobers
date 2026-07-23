package gate

import (
	"context"
	"fmt"
	"time"

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
	// Outcome is the evaluator outcome (a check's "pass"/"fail", or an
	// agentic Verdict's Decision string), or the synthesized fail-closed
	// outcome for an interrupted-budget escalation.
	Outcome string
	// Target is the branch actually taken — the gate's configured branch for
	// Outcome, unless the repass budget was exhausted, in which case it is the
	// optional escalate control branch or workflow.TargetEscalate.
	Target string
	// Attempt is this gate's consecutive non-pass evaluation count,
	// including this one when Outcome != OutcomePass (0 when Outcome ==
	// OutcomePass, since a pass resets the budget).
	Attempt int
	// Escalated is true when Target was overridden by the repass budget
	// rather than resolved from the gate's own Branches.
	Escalated bool
	// DuplicateDiff is true when Escalated fired because this attempt's diff
	// digest matched the immediately prior attempt's (issue #316), rather
	// than because the repass budget was exhausted. The reviewer was never
	// called for this attempt — Verdict is synthesized, not agent-produced.
	DuplicateDiff bool
	// CacheHit is true when Evaluator.CachedVerdict was set for this
	// evaluation (issue #523's cross-run verdict cache): the reviewer was
	// never called, and Verdict is the caller-supplied cached verdict,
	// re-journaled as-is (including its original SourceRunID) rather than
	// freshly produced. Distinct from DuplicateDiff, which is an in-run,
	// same-attempt-content dedup — CacheHit is cross-run, keyed by the
	// caller's own digest match (the caller — merge-review's
	// gather-sibling-context — already verified the cached verdict's
	// inputs are unchanged before ever setting CachedVerdict; Evaluate
	// trusts that verification and never recomputes it).
	CacheHit bool
	// Interrupted is true when a recovered, dangling gate.started marker had
	// already consumed enough repass slots to force escalation. The evaluator
	// is not invoked again for this synthesized, fail-closed result.
	Interrupted bool
	// Verdict is the full agentic-gate verdict (decision, rationale,
	// evidence, findings). nil for automated gates.
	Verdict *apiv1.Verdict
	// VerdictArtifact points at Verdict as journaled (recordVerdict,
	// journal.go — the same "verdict/<gate>-<attempt>.json" artifact
	// DuplicateDiff's synthesized verdict is also journaled as). nil
	// whenever Verdict is nil, or when Journal is nil (journaling
	// disabled). The runner surfaces this as a ContextPointer on a repass
	// dispatch (issue #412) so the reimplementing stage actually receives
	// the reviewer's rationale — the same content this gate itself already
	// persisted — instead of re-inferring "something needs to change" from
	// git alone.
	VerdictArtifact *apiv1.ArtifactPointer
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
	// reconstructing it from each gate's last gate.started/gate.evaluated
	// event (Runner["repassAttempt"], journal.go), the same source state.json
	// itself is always reconstructable from. Evaluate mutates this map in
	// place, so it also serves as the live, inspectable checkpoint source for
	// a caller that wants to persist it after each gate — read
	// Attempts[gateName], not Result.Attempt, if a pass may have reset it
	// since the last read. Nil-safe: Evaluate lazily allocates it on first
	// use. Exported instead of a constructor because every other Evaluator
	// field is already set via struct literal (see internal/runner/run.go).
	Attempts map[string]int

	// LastDiffDigest holds each agentic gate's most recently evaluated diff
	// digest, keyed by gate name (issue #316: an implementer stuck in a
	// non-convergent repass loop can produce byte-identical diffs attempt
	// after attempt, burning the whole repass budget on reviewer calls that
	// can only repeat their prior verdict). The caller (run.go's
	// evaluateGate) passes each attempt's digest into Evaluate as
	// diffDigest — the same content-addressed digest already computed for
	// the reviewer's evidence artifact (recordReviewerDiff), never
	// recomputed here. A match against the stored digest short-circuits the
	// reviewer call and escalates immediately (Result.DuplicateDiff).
	// Mirrors Attempts' seeding contract exactly: nil is the correct zero
	// value for a fresh run, a resuming caller seeds it from the journal
	// (Runner["diffDigest"] on each gate's last gate.evaluated event,
	// internal/runner/resume.go's gateDiffSeed), and Evaluate mutates it in
	// place as the live checkpoint source.
	LastDiffDigest map[string]string

	// CachedVerdict, when non-nil, short-circuits the NEXT agentic gate
	// Evaluate call: the reviewer is never invoked, and this Verdict is
	// reused as-is (Result.CacheHit = true). Ignored for automated/human
	// gates. Rebound fresh by the caller before every Evaluate call — the
	// same mutate-before-call contract Reviewer already documents above
	// (evaluateGate sets it, possibly to nil, on every gate dispatch) — so
	// a cache hit for one gate can never leak into the next gate this
	// Evaluator evaluates. Issue #523: merge-review's review gate is the
	// only caller that ever sets this, from a digest-matched verdict
	// gather-sibling-context already found on the selected PR's own prior
	// comment (or, within the same run, on this run's own journal) —
	// scoped there, not here, precisely so this stays a generic,
	// workflow-agnostic mechanism: Evaluate itself has no notion of PRs,
	// siblings, or digests, only "a caller-verified verdict is available,
	// reuse it."
	CachedVerdict *apiv1.Verdict
}

// Evaluate runs gate g's evaluator against env (already built by the caller,
// including — for automated gates — the flattened Inputs convention
// documented in automated.go), attaches subject's artifacts as evidence for
// agentic gates, resolves the branch via the compiled machine's Branches
// (workflow.Compile already validated every branch target resolves to a real
// state or a reserved terminal), enforces the repass budget, and journals the
// result.
//
// diffDigest is the content-addressed digest of this attempt's committed
// diff (run.go's recordReviewerDiff — already computed for the reviewer's
// own evidence artifact, never recomputed here), or "" when the caller has
// none (automated/human gates, or an agentic gate whose branch carries no
// diff at all). For an agentic gate, a non-empty diffDigest that matches the
// gate's previously recorded digest (LastDiffDigest) means this attempt's
// diff is byte-identical to the immediately prior one — the reviewer already
// judged this exact change and a repeat call can only repeat that verdict,
// so Evaluate skips the (real, costly) reviewer call and escalates
// immediately instead of burning the rest of the repass budget on attempts
// that cannot converge (issue #316).
// emptyDiff (issue #415, reviewer sibling of the non-retryable escalate
// route) is true when an agentic gate's subject branch carries no committed
// change at all — the caller (run.go's evaluateGate) knows this unambiguously
// because recordReviewerDiff returns a nil pointer for a zero-length diff. An
// empty diff offers the reviewer nothing to evaluate and a repass nothing to
// iterate on, so Evaluate fast-`fail`s it on the first review (resolving the
// gate's own `fail` branch) instead of spending real reviewer calls and repass
// cycles that can only re-observe the same emptiness. Ignored for
// automated/human gates. Distinct from diffDigest, which the tests set to ""
// to mean "no digest supplied, still call the reviewer" — emptiness is an
// explicit signal, never inferred from an empty digest.
func (e *Evaluator) Evaluate(ctx context.Context, g apiv1.Gate, env apiv1.InvocationEnvelope, subjectStage string, subject apiv1.ResultEnvelope, diffDigest string, emptyDiff bool) (Result, error) {
	if r, recovered, err := e.RecoverInterrupted(g, diffDigest); err != nil || recovered {
		return r, err
	}

	switch g.Evaluator {
	case apiv1.EvaluatorAutomated:
		if e.Automated == nil {
			return Result{}, fmt.Errorf("gate %q: automated evaluator not configured", g.Name)
		}
	case apiv1.EvaluatorAgentic:
		if e.CachedVerdict == nil && e.Reviewer == nil {
			return Result{}, fmt.Errorf("gate %q: agentic reviewer not configured", g.Name)
		}
	case apiv1.EvaluatorHuman:
		return Result{}, fmt.Errorf("gate %q: human evaluator is not supported at V0 (GT-003, ships V1)", g.Name)
	default:
		return Result{}, fmt.Errorf("gate %q: unknown evaluator %q", g.Name, g.Evaluator)
	}

	if err := recordStart(e.Journal, g.Name, e.Attempts[g.Name]+1); err != nil {
		return Result{}, fmt.Errorf("gate %q: journal evaluation start: %w", g.Name, err)
	}
	// #765: the evaluator call below is invoked through evaluateWithRetry, which
	// honors the gate's declared RetryPolicy for transient evaluator errors and
	// applies the per-attempt timeout (env.Limits.MaxDurationSeconds, the gate's
	// own TimeoutSeconds) inside each attempt so a retry gets a fresh window
	// rather than the remainder of a shared one.
	policy := gateRetryPolicy(g)

	var outcome string
	var verdict *apiv1.Verdict
	duplicateDiff := false
	cacheHit := false

	switch g.Evaluator {
	case apiv1.EvaluatorAutomated:
		conf := apiv1.AutomatedGate{}
		if g.Automated != nil {
			conf = *g.Automated
		}
		var out string
		if err := e.evaluateWithRetry(ctx, g.Name, policy, env.Limits.MaxDurationSeconds, func(attemptCtx context.Context) error {
			var callErr error
			out, callErr = e.Automated.Evaluate(attemptCtx, conf, env)
			return callErr
		}); err != nil {
			return Result{}, fmt.Errorf("gate %q: evaluate automated: %w", g.Name, err)
		}
		outcome = out

	case apiv1.EvaluatorAgentic:
		if e.CachedVerdict != nil {
			// #523: the caller already found a digest-matched verdict for
			// this exact evaluation's inputs (see CachedVerdict's doc
			// comment) — checked first, ahead of the e.Reviewer nil-guard
			// below (a cache-hit caller has no reason to construct/wire a
			// reviewer goober it already knows Evaluate won't invoke, so
			// Reviewer may legitimately be nil here) and ahead of
			// emptyDiff/duplicateDiff (same-run repass heuristics inferring
			// what the reviewer would likely say, whereas this is the
			// caller asserting it already knows: the real answer, computed
			// against these identical inputs, already exists).
			cacheHit = true
			verdict = e.CachedVerdict
			outcome = string(verdict.Decision)
		} else if emptyDiff {
			// #415 sibling: the implement stage produced no committed change,
			// so there is nothing for the reviewer to evaluate or a repass to
			// iterate on. Fast-`fail` on the first review — resolving the
			// gate's own `fail` branch (attempt 1, so no escalation) — instead
			// of issuing needs-changes and burning repass cycles that can only
			// re-observe the same empty diff. Mirrors the identical-diff guard
			// below: both spare the repass budget a degenerate reviewer call.
			outcome = string(apiv1.VerdictFail)
			verdict = &apiv1.Verdict{
				Decision:  apiv1.VerdictFail,
				Rationale: "runner: the implement stage produced no committed changes — failing without review, since an empty diff offers nothing to evaluate and a repass can only reproduce it",
			}
		} else if diffDigest != "" && e.LastDiffDigest != nil && e.LastDiffDigest[g.Name] == diffDigest {
			duplicateDiff = true
			outcome = string(apiv1.VerdictNeedsChanges)
			verdict = &apiv1.Verdict{
				Decision:  apiv1.VerdictNeedsChanges,
				Rationale: fmt.Sprintf("runner: this repass produced a diff identical to the immediately prior attempt (digest %s) — escalating without re-review, since an unchanged diff cannot yield a different verdict", diffDigest),
			}
		} else {
			var v apiv1.Verdict
			if err := e.evaluateWithRetry(ctx, g.Name, policy, env.Limits.MaxDurationSeconds, func(attemptCtx context.Context) error {
				var callErr error
				v, callErr = e.Reviewer.Review(attemptCtx, env, subjectStage, subject)
				return callErr
			}); err != nil {
				return Result{}, fmt.Errorf("gate %q: reviewer evaluation: %w", g.Name, err)
			}
			verdict = &v
			outcome = string(v.Decision)
		}

	}

	return e.resolveOutcome(g, outcome, verdict, diffDigest, duplicateDiff, cacheHit)
}

// EvaluateKnownOutcome applies the gate's branch and repass policy to an
// outcome already established by the runner without dispatching an evaluator.
func (e *Evaluator) EvaluateKnownOutcome(g apiv1.Gate, outcome string) (Result, error) {
	if r, recovered, err := e.RecoverInterrupted(g, ""); err != nil || recovered {
		return r, err
	}
	if g.Evaluator != apiv1.EvaluatorAutomated {
		return Result{}, fmt.Errorf("gate %q: only automated gates accept a known outcome", g.Name)
	}
	if err := recordStart(e.Journal, g.Name, e.Attempts[g.Name]+1); err != nil {
		return Result{}, fmt.Errorf("gate %q: journal evaluation start: %w", g.Name, err)
	}
	return e.resolveOutcome(g, outcome, nil, "", false, false)
}

func (e *Evaluator) resolveOutcome(g apiv1.Gate, outcome string, verdict *apiv1.Verdict, diffDigest string, duplicateDiff, cacheHit bool) (Result, error) {
	if diffDigest != "" {
		if e.LastDiffDigest == nil {
			e.LastDiffDigest = make(map[string]string)
		}
		e.LastDiffDigest[g.Name] = diffDigest
	}

	target, ok := wf.BranchTarget(g, outcome)
	if !ok {
		return Result{}, fmt.Errorf("gate %q: outcome %q has no defined branch (never a silent pass, GT-002)", g.Name, outcome)
	}

	attempt, exceeded := e.trackRepass(g.Name, outcome)
	escalated := exceeded || duplicateDiff
	if escalated {
		target = escalationTarget(g)
	}

	r := Result{Gate: g.Name, Outcome: outcome, Target: target, Attempt: attempt, Escalated: escalated, DuplicateDiff: duplicateDiff, CacheHit: cacheHit, Verdict: verdict}
	artifact, err := recordVerdict(e.Journal, r, diffDigest)
	if err != nil {
		return Result{}, fmt.Errorf("gate %q: journal verdict: %w", g.Name, err)
	}
	r.VerdictArtifact = artifact
	return r, nil
}

// RecoverInterrupted synthesizes and journals the terminal escalation required
// when restored dangling gate.started markers have already exhausted a gate's
// repass budget. Callers must check this before preparing a side-effecting
// evaluator; Evaluate also checks it as a fail-safe for direct callers.
func (e *Evaluator) RecoverInterrupted(g apiv1.Gate, diffDigest string) (Result, bool, error) {
	attempt := e.Attempts[g.Name]
	if attempt <= e.maxRepasses() {
		return Result{}, false, nil
	}
	r := Result{
		Gate:        g.Name,
		Outcome:     OutcomeFail,
		Target:      escalationTarget(g),
		Attempt:     attempt,
		Escalated:   true,
		Interrupted: true,
	}
	artifact, err := recordVerdict(e.Journal, r, diffDigest)
	if err != nil {
		return Result{}, true, fmt.Errorf("gate %q: journal interrupted escalation: %w", g.Name, err)
	}
	r.VerdictArtifact = artifact
	return r, true, nil
}

func escalationTarget(g apiv1.Gate) string {
	if target, ok := wf.BranchTarget(g, wf.BranchEscalate); ok {
		return target
	}
	return wf.TargetEscalate
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
	return attempt, attempt > e.maxRepasses()
}

func (e *Evaluator) maxRepasses() int {
	if e.MaxRepasses > 0 {
		return e.MaxRepasses
	}
	return DefaultMaxRepasses
}

// gateRetryPolicy returns the gate's declared evaluator retry policy, read off
// its evaluator sub-config (#151 added the DSL field on AutomatedGate/
// AgenticGate; #765 honors it). nil when the gate declares no retry — the
// common case, which evaluateWithRetry treats as a single attempt.
func gateRetryPolicy(g apiv1.Gate) *apiv1.RetryPolicy {
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

// retryBounds resolves a RetryPolicy into a total attempt count and constant
// backoff. A nil policy — or MaxAttempts <= 1 — means a single attempt, so a
// gate that declares no retry is byte-identical to the pre-#765 fail-fast
// behavior. This is what bounds the blast radius: only a gate that opts in via
// `retry:` ever retries.
func retryBounds(policy *apiv1.RetryPolicy) (maxAttempts int, backoff time.Duration) {
	maxAttempts = 1
	if policy != nil && policy.MaxAttempts > 1 {
		maxAttempts = int(policy.MaxAttempts)
		backoff = time.Duration(policy.BackoffSeconds) * time.Second
	}
	return maxAttempts, backoff
}

// evaluateWithRetry invokes a gate's evaluator (call) up to the bound declared
// by policy, retrying ONLY transient errors — those an evaluator seam marked
// invoke.InfrastructureFailure, the same predicate the runner's stage-retry
// path uses (internal/runner, run.go). The #765 case is a reviewer session that
// wrote no verdict file, tagged infrastructure by the harness Executor or, for
// custom Goober implementations, the reviewer seam. A non-transient error — a
// misconfiguration, a business failure, anything unmarked — returns immediately,
// and exhausting the bound
// returns the last error: both fail the run exactly as before #765. Each failed
// transient attempt is journaled (recordEvaluatorRetry), and each attempt runs
// under its own timeoutSeconds deadline so a retry gets a fresh window.
func (e *Evaluator) evaluateWithRetry(ctx context.Context, gateName string, policy *apiv1.RetryPolicy, timeoutSeconds int32, call func(context.Context) error) error {
	maxAttempts, backoff := retryBounds(policy)
	for attempt := 1; ; attempt++ {
		attemptCtx := ctx
		var cancel context.CancelFunc
		if timeoutSeconds > 0 {
			attemptCtx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
		}
		err := call(attemptCtx)
		if cancel != nil {
			cancel()
		}
		if err == nil {
			return nil
		}
		// Only a transient (infrastructure-marked) error is retryable; a
		// non-transient one fails fast, no wasted retries.
		if !invoke.IsInfrastructureFailure(err) {
			return err
		}
		if jerr := recordEvaluatorRetry(e.Journal, gateName, attempt, err); jerr != nil {
			return jerr
		}
		if attempt >= maxAttempts {
			// Bound exhausted — fail the run, never a silent infinite retry.
			return err
		}
		if backoff > 0 {
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return err
			case <-timer.C:
			}
		}
	}
}
