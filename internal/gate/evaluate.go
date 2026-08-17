package gate

import (
	"context"
	"fmt"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/runcontrol"
	wf "github.com/goobers/goobers/internal/workflow"
)

// DefaultMaxRepasses is the default bounded-repass budget: a stage may be
// re-entered from a gate this many times before Evaluator overrides the gate's
// configured branch and routes to workflow.TargetEscalate instead.
// Inherited run policy may override this default, and Gate.MaxRepasses may
// override the inherited value for one automated or agentic gate.
const DefaultMaxRepasses = runcontrol.DefaultMaxRepasses

// Result is the outcome of one gate evaluation.
type Result struct {
	// Gate is the evaluated gate's name.
	Gate string
	// Actor identifies the human principal that supplied a human-gate
	// decision. Empty for automated and agentic gates.
	Actor string
	// Outcome is the evaluator outcome (a check's "pass"/"fail", or an
	// agentic Verdict's Decision string), or the synthesized fail-closed
	// outcome for an interrupted-budget escalation.
	Outcome string
	// Target is the branch actually taken — the gate's configured branch for
	// Outcome, unless the repass budget was exhausted, in which case it is the
	// optional escalate control branch or workflow.TargetEscalate.
	Target string
	// Attempt is the number of times the repass target has been re-entered,
	// including this evaluation. It is 0 when the outcome does not repass.
	Attempt int
	// RepassTarget is the configured branch target charged by Attempt. It
	// remains set when budget exhaustion overrides Target with escalation.
	RepassTarget string
	// GateAttempt is this gate's consecutive non-pass evaluation count. It is
	// retained separately to recover dangling gate evaluations after a crash.
	GateAttempt int
	// Escalated is true when Target was overridden by the runner because the
	// repass budget was exhausted or evaluation cannot make progress.
	Escalated bool
	// DuplicateDiff is true when Escalated fired because this attempt's diff
	// digest matched the immediately prior attempt's (issue #316), rather
	// than because the repass budget was exhausted. The reviewer was never
	// called for this attempt — Verdict is synthesized, not agent-produced.
	DuplicateDiff bool
	// RepassCause identifies the gate or stage failure that sent the run back
	// to the subject stage before DuplicateDiff detected no resulting change.
	RepassCause *RepassCause
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

// RepassCause is the machine-readable upstream reason for a repass.
type RepassCause struct {
	Kind         string `json:"kind"`
	Gate         string `json:"gate,omitempty"`
	Outcome      string `json:"outcome,omitempty"`
	Stage        string `json:"stage,omitempty"`
	ErrorCode    string `json:"errorCode,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
	Rationale    string `json:"rationale,omitempty"`
}

func (c RepassCause) String() string {
	switch c.Kind {
	case "stage-failure":
		detail := ""
		if c.ErrorCode != "" {
			detail = fmt.Sprintf(" (code %q)", c.ErrorCode)
		}
		if c.ErrorMessage != "" {
			detail += ": " + c.ErrorMessage
		}
		return fmt.Sprintf("the prior repass was triggered by gate %q outcome %q after stage %q failed%s", c.Gate, c.Outcome, c.Stage, detail)
	case "reviewer":
		detail := ""
		if c.Rationale != "" {
			detail = ": " + c.Rationale
		}
		return fmt.Sprintf("the prior repass was triggered by reviewer gate %q returning %q%s", c.Gate, c.Outcome, detail)
	default:
		return fmt.Sprintf("the prior repass was triggered by gate %q outcome %q", c.Gate, c.Outcome)
	}
}

// Evaluator dispatches automated and agentic gates and resolves explicit human
// decisions, maps outcomes to branches via the compiled machine, enforces the
// bounded-repass budget, and journals the verdict. It is safe for reuse across
// every gate evaluation within a single run; it is NOT safe for concurrent use
// (a run advances one state at a time) and MUST NOT be shared across runs
// (repass counts are per-run state).
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
	// MaxRepasses is the inherited run budget. Gate.MaxRepasses takes precedence.
	MaxRepasses int

	// Attempts holds each gate's consecutive non-pass count for recovering
	// crash-interrupted evaluations. Budget enforcement uses RepassAttempts.
	// A resuming caller reconstructs this map from gateAttempt annotations.
	Attempts map[string]int

	// RepassAttempts holds cumulative repasses keyed by configured branch
	// target. Unlike Attempts, a pass at one gate does not reset another gate's
	// budget for re-entering the same stage. A resuming caller reconstructs it
	// from repassTarget and repassAttempt annotations.
	RepassAttempts map[string]int

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

	// RepassCause describes the transition that dispatched the subject stage
	// most recently. The runner rebinds it before each evaluation.
	RepassCause *RepassCause

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
		return Result{}, fmt.Errorf("gate %q: human evaluator requires an explicit decision", g.Name)
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
			rationale := fmt.Sprintf("runner: this repass produced no change (digest %s)", diffDigest)
			if e.RepassCause != nil {
				rationale = e.RepassCause.String() + "; the implementer produced no change in response"
			}
			verdict = &apiv1.Verdict{
				Decision:  apiv1.VerdictNeedsChanges,
				Rationale: rationale,
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

	return e.resolveOutcome(g, outcome, verdict, diffDigest, duplicateDiff, emptyDiff, cacheHit)
}

// EvaluateHuman applies an explicit human decision to a human gate. The
// actor must match a configured approver when the gate restricts approvers, and
// the decision must exactly match a configured branch. Human gates execute
// nothing, so there is no pre-dispatch gate.started marker.
func (e *Evaluator) EvaluateHuman(g apiv1.Gate, decision, actor string) (Result, error) {
	if err := ValidateHumanDecision(g, decision, actor); err != nil {
		return Result{}, err
	}
	target, _ := wf.BranchTarget(g, decision)
	r := Result{Gate: g.Name, Actor: actor, Outcome: decision, Target: target}
	if _, err := recordVerdict(e.Journal, r, ""); err != nil {
		return Result{}, fmt.Errorf("gate %q: journal verdict: %w", g.Name, err)
	}
	return r, nil
}

// ValidateHumanDecision verifies a human-gate decision without mutating its
// journal. The runner uses it before resuming so invalid external input cannot
// fail an otherwise healthy paused run.
func ValidateHumanDecision(g apiv1.Gate, decision, actor string) error {
	if g.Evaluator != apiv1.EvaluatorHuman {
		return fmt.Errorf("gate %q: only human gates accept a human decision", g.Name)
	}
	if g.Human != nil && len(g.Human.Approvers) > 0 {
		if actor == "" {
			return fmt.Errorf("gate %q: human decision actor is required by approver restrictions", g.Name)
		}
		authorized := false
		for _, approver := range g.Human.Approvers {
			if actor == approver {
				authorized = true
				break
			}
		}
		if !authorized {
			return fmt.Errorf("gate %q: actor %q is not an authorized approver", g.Name, actor)
		}
	}
	if decision == "" {
		return fmt.Errorf("gate %q: human decision is required", g.Name)
	}
	_, ok := wf.BranchTarget(g, decision)
	if !ok {
		return fmt.Errorf("gate %q: decision %q has no defined branch (never a silent pass, GT-002)", g.Name, decision)
	}
	return nil
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
	return e.resolveOutcome(g, outcome, nil, "", false, false, false)
}

func (e *Evaluator) resolveOutcome(g apiv1.Gate, outcome string, verdict *apiv1.Verdict, diffDigest string, duplicateDiff, forcedEscalation, cacheHit bool) (Result, error) {
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

	attempt, gateAttempt, repassTarget, exceeded := e.trackRepass(g, outcome, target)
	escalated := exceeded || duplicateDiff || forcedEscalation
	if escalated {
		target = escalationTarget(g)
	}

	var repassCause *RepassCause
	if duplicateDiff {
		repassCause = e.RepassCause
	}
	r := Result{
		Gate: g.Name, Outcome: outcome, Target: target, Attempt: attempt,
		RepassTarget: repassTarget, GateAttempt: gateAttempt, Escalated: escalated,
		DuplicateDiff: duplicateDiff, RepassCause: repassCause, CacheHit: cacheHit, Verdict: verdict,
	}
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
	if attempt <= e.maxRepasses(g) {
		return Result{}, false, nil
	}
	r := Result{
		Gate:        g.Name,
		Outcome:     OutcomeFail,
		Target:      escalationTarget(g),
		Attempt:     attempt,
		GateAttempt: attempt,
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

// trackRepass charges non-pass branches to their target stage. The per-gate
// counter remains for interrupted-evaluation recovery, but budget enforcement
// uses the target counter so distinct gates cannot grant each other fresh
// repass budgets.
func (e *Evaluator) trackRepass(g apiv1.Gate, outcome, target string) (attempt, gateAttempt int, repassTarget string, exceeded bool) {
	if e.Attempts == nil {
		e.Attempts = make(map[string]int)
	}
	if outcome == OutcomePass {
		e.Attempts[g.Name] = 0
		return 0, 0, "", false
	}
	e.Attempts[g.Name]++
	gateAttempt = e.Attempts[g.Name]
	if e.RepassAttempts == nil {
		e.RepassAttempts = make(map[string]int)
	}
	e.RepassAttempts[target]++
	attempt = e.RepassAttempts[target]
	return attempt, gateAttempt, target, attempt > e.maxRepasses(g)
}

func (e *Evaluator) maxRepasses(g apiv1.Gate) int {
	return runcontrol.MaxRepassesForGate(g, e.MaxRepasses)
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
