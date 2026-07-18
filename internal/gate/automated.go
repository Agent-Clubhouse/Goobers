package gate

import (
	"context"
	"fmt"
	"regexp"
	"strconv"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
)

// InputKeyStatus is the env.Inputs key an automated gate reads the subject
// stage's status from (runner contract, below).
//
// Convention (runner contract): an automated gate evaluates the subject
// stage's normalized result without ever seeing the stage's ResultEnvelope
// body directly (ARCHITECTURE.md §2.4 forbids that reach-through). Instead the
// runner, when it builds the gate's InvocationEnvelope, MUST flatten the
// subject stage's small, scalar-only signal into env.Inputs:
//
//   - env.Inputs[InputKeyStatus] = string(subject.Status)  ("success"/"failure"/"blocked")
//   - every k/v in subject.Outputs copied into env.Inputs as-is (Outputs are
//     already documented as "small, named scalar values downstream
//     stages/gates can consume directly" — api/v1alpha1.ResultEnvelope)
//
// This keeps the checker registry pure (no journal/filesystem access) and
// keeps the expression surface intentionally minimal: named checks over a flat
// key/value map, not a general expression language.
const InputKeyStatus = "status"

// Outcome values an automated check returns. The gate's Branches map treats
// "pass" as the success branch (api/v1alpha1.Gate.Branches doc comment); any
// other outcome is a caller-defined branch name (conventionally "fail").
const (
	OutcomePass = "pass"
	OutcomeFail = "fail"
)

// OutcomeTimeout is the "ci-status" check's third outcome (#239) — distinct
// from pass/fail — when the polled ciStatus is executor.CIStatusTimeout: the
// poll itself never reached a terminal passing/failing state before its
// deadline, which is different evidence from "CI ran and failed" and must
// not resolve through the same "fail" branch a workflow definition wires to
// an implement repass. Unlike a bare pass/fail check, this third outcome is
// enforced at compile time, not just evaluation time: internal/workflow's
// gateOutcomeProblems (compile.go's automatedCheckOutcomes table) treats
// "timeout" as one of ci-status's producible outcomes, so a ci-status gate
// missing a "timeout" branch fails Compile outright (GT-002) rather than
// compiling clean and only failing closed the first time a real poll times
// out. Every in-tree ci-status gate (acme-web/selfhost/testdata) declares
// one.
const OutcomeTimeout = "timeout"

// OutcomeMerged/OutcomeEnqueued are "land-outcome"'s two success outcomes
// (issue #758): a merge-pr stage that actually merged the pull request
// reports OutcomeMerged; one that added it to the repo's merge queue
// instead (merge-policy abstraction, internal/mergepolicy) reports
// OutcomeEnqueued. Neither is a plain "pass" — merge-review's merge-gate
// must route the two differently (post-merge's close-referenced-issues fan
// out only makes sense once something is actually merged; an enqueued pull
// request instead needs its merge-queue entry watched to a terminal
// outcome), so this, like ci-status's OutcomeTimeout, is enforced at
// compile time via automatedCheckOutcomes rather than left to fail closed
// only the first time a workflow definition's gate misses a branch.
const (
	OutcomeMerged   = "merged"
	OutcomeEnqueued = "enqueued"
)

// OutcomeEvicted is "queue-outcome"'s explicit eviction outcome (issue
// #758): a merge queue that removed a previously-enqueued pull request
// without merging it (its combined build against the projected merge state
// failed) — a first-class outcome a workflow definition can route to
// remediation, never silently conflated with "fail" or "timeout".
const OutcomeEvicted = "evicted"

// CheckFunc evaluates one named automated check against a gate's flattened
// Inputs and its configured Params, returning an outcome ("pass"/"fail" for
// the checks in DefaultChecks, though a custom check may return any outcome
// string the gate's Branches map declares).
type CheckFunc func(inputs map[string]interface{}, params map[string]string) (outcome string, err error)

// DefaultChecks is the minimal, documented set of automated checks available
// to a gate via AutomatedGate.Check. Each check's Params contract is
// documented on its entry below.
func DefaultChecks() map[string]CheckFunc {
	return map[string]CheckFunc{
		// "status-equals": pass iff Inputs[status] == Params["equals"]
		// (default "success"). Covers "branch on exit status".
		"status-equals": func(inputs map[string]interface{}, params map[string]string) (string, error) {
			want := params["equals"]
			if want == "" {
				want = string(apiv1.ResultSuccess)
			}
			return boolOutcome(stringField(inputs, InputKeyStatus) == want), nil
		},
		// "output-equals": pass iff Inputs[Params["key"]] stringifies to
		// Params["equals"]. Both params required.
		"output-equals": func(inputs map[string]interface{}, params map[string]string) (string, error) {
			key, ok := params["key"]
			if !ok || key == "" {
				return "", fmt.Errorf("gate: check %q requires params.key", "output-equals")
			}
			want, ok := params["equals"]
			if !ok {
				return "", fmt.Errorf("gate: check %q requires params.equals", "output-equals")
			}
			return boolOutcome(stringField(inputs, key) == want), nil
		},
		// "output-numeric-gte": pass iff the numeric value of
		// Inputs[Params["key"]] is >= Params["threshold"]. Covers "coverage
		// >= X"-style checks. Both params required; non-numeric values error
		// rather than silently failing closed on a misconfigured gate.
		"output-numeric-gte": func(inputs map[string]interface{}, params map[string]string) (string, error) {
			key, ok := params["key"]
			if !ok || key == "" {
				return "", fmt.Errorf("gate: check %q requires params.key", "output-numeric-gte")
			}
			thresholdStr, ok := params["threshold"]
			if !ok {
				return "", fmt.Errorf("gate: check %q requires params.threshold", "output-numeric-gte")
			}
			threshold, err := strconv.ParseFloat(thresholdStr, 64)
			if err != nil {
				return "", fmt.Errorf("gate: check %q: params.threshold %q: %w", "output-numeric-gte", thresholdStr, err)
			}
			got, err := numericField(inputs, key)
			if err != nil {
				return "", fmt.Errorf("gate: check %q: %w", "output-numeric-gte", err)
			}
			return boolOutcome(got >= threshold), nil
		},
		// "output-numeric-lte": pass iff the numeric value of
		// Inputs[Params["key"]] is <= Params["threshold"]. Both params
		// required; non-numeric values error.
		"output-numeric-lte": func(inputs map[string]interface{}, params map[string]string) (string, error) {
			key, ok := params["key"]
			if !ok || key == "" {
				return "", fmt.Errorf("gate: check %q requires params.key", "output-numeric-lte")
			}
			thresholdStr, ok := params["threshold"]
			if !ok {
				return "", fmt.Errorf("gate: check %q requires params.threshold", "output-numeric-lte")
			}
			threshold, err := strconv.ParseFloat(thresholdStr, 64)
			if err != nil {
				return "", fmt.Errorf("gate: check %q: params.threshold %q: %w", "output-numeric-lte", thresholdStr, err)
			}
			got, err := numericField(inputs, key)
			if err != nil {
				return "", fmt.Errorf("gate: check %q: %w", "output-numeric-lte", err)
			}
			return boolOutcome(got <= threshold), nil
		},
		// "output-numeric-lt": pass iff the numeric value of
		// Inputs[Params["key"]] is < Params["threshold"]. Both params
		// required; non-numeric values error.
		"output-numeric-lt": func(inputs map[string]interface{}, params map[string]string) (string, error) {
			key, ok := params["key"]
			if !ok || key == "" {
				return "", fmt.Errorf("gate: check %q requires params.key", "output-numeric-lt")
			}
			thresholdStr, ok := params["threshold"]
			if !ok {
				return "", fmt.Errorf("gate: check %q requires params.threshold", "output-numeric-lt")
			}
			threshold, err := strconv.ParseFloat(thresholdStr, 64)
			if err != nil {
				return "", fmt.Errorf("gate: check %q: params.threshold %q: %w", "output-numeric-lt", thresholdStr, err)
			}
			got, err := numericField(inputs, key)
			if err != nil {
				return "", fmt.Errorf("gate: check %q: %w", "output-numeric-lt", err)
			}
			return boolOutcome(got < threshold), nil
		},
		// "output-not-equals": pass iff Inputs[Params["key"]] stringifies
		// to a value other than Params["equals"]. Both params required.
		"output-not-equals": func(inputs map[string]interface{}, params map[string]string) (string, error) {
			key, ok := params["key"]
			if !ok || key == "" {
				return "", fmt.Errorf("gate: check %q requires params.key", "output-not-equals")
			}
			want, ok := params["equals"]
			if !ok {
				return "", fmt.Errorf("gate: check %q requires params.equals", "output-not-equals")
			}
			got, err := outputStringField(inputs, key)
			if err != nil {
				return "", fmt.Errorf("gate: check %q: %w", "output-not-equals", err)
			}
			return boolOutcome(got != want), nil
		},
		// "output-matches": pass iff Inputs[Params["key"]] stringifies to
		// a value matching the RE2 Params["pattern"]. Both params required.
		"output-matches": func(inputs map[string]interface{}, params map[string]string) (string, error) {
			key, ok := params["key"]
			if !ok || key == "" {
				return "", fmt.Errorf("gate: check %q requires params.key", "output-matches")
			}
			pattern, ok := params["pattern"]
			if !ok {
				return "", fmt.Errorf("gate: check %q requires params.pattern", "output-matches")
			}
			re, err := regexp.Compile(pattern)
			if err != nil {
				return "", fmt.Errorf("gate: check %q: params.pattern %q: %w", "output-matches", pattern, err)
			}
			got, err := outputStringField(inputs, key)
			if err != nil {
				return "", fmt.Errorf("gate: check %q: %w", "output-matches", err)
			}
			return boolOutcome(re.MatchString(got)), nil
		},
		// "ci-status": pass iff Inputs["ciStatus"] (the well-known output key
		// a ci-poll deterministic stage — issue #18 — is expected to set,
		// using the providers.CheckState vocabulary "passing"/"failing")
		// equals Params["equals"] (default "passing"). Prior to #132 this
		// defaulted to apiv1.ResultStatus's "success", which a ci-poll stage
		// emitting "passing"/"failing" could never match.
		//
		// A ciStatus of executor.CIStatusTimeout ("timeout", #239) is
		// reported as its own OutcomeTimeout outcome rather than folded into
		// "fail": a poll that never reached a terminal state is not the same
		// evidence as CI actually failing, and a workflow definition's
		// ci-gate must be free to route it to escalation instead of the
		// "fail" branch's implement repass.
		"ci-status": func(inputs map[string]interface{}, params map[string]string) (string, error) {
			want := params["equals"]
			if want == "" {
				want = "passing"
			}
			got := stringField(inputs, "ciStatus")
			if got == executor.CIStatusTimeout {
				return OutcomeTimeout, nil
			}
			return boolOutcome(got == want), nil
		},
		// "land-outcome": reports merge-pr's Outputs["landOutcome"] (issue
		// #758) — "merged" or "enqueued" when merge-pr actually attempted a
		// landing via internal/mergepolicy's Land, or "fail" for every
		// other case (an unmet merge conjunct, advisory mode — merge-pr
		// sets neither merged=true nor landOutcome then, matching its
		// existing "not merged" writeback). No params.
		"land-outcome": func(inputs map[string]interface{}, params map[string]string) (string, error) {
			switch stringField(inputs, "landOutcome") {
			case OutcomeMerged:
				return OutcomeMerged, nil
			case OutcomeEnqueued:
				return OutcomeEnqueued, nil
			default:
				return OutcomeFail, nil
			}
		},
		// "queue-outcome": reports merge-queue-poll's
		// Outputs["queueOutcome"] (issue #758) — "merged", "evicted", or
		// "timeout" (mirroring ci-status's own third-outcome shape, #239,
		// for "still pending past this stage's own bounded poll"), or
		// "fail" for anything else (a misconfigured/absent value —
		// merge-queue-poll itself always sets one of the first three). No
		// params.
		"queue-outcome": func(inputs map[string]interface{}, params map[string]string) (string, error) {
			switch stringField(inputs, "queueOutcome") {
			case OutcomeMerged:
				return OutcomeMerged, nil
			case OutcomeEvicted:
				return OutcomeEvicted, nil
			case OutcomeTimeout:
				return OutcomeTimeout, nil
			default:
				return OutcomeFail, nil
			}
		},
	}
}

func boolOutcome(pass bool) string {
	if pass {
		return OutcomePass
	}
	return OutcomeFail
}

func stringField(inputs map[string]interface{}, key string) string {
	v, ok := inputs[key]
	if !ok {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func outputStringField(inputs map[string]interface{}, key string) (string, error) {
	v, ok := inputs[key]
	if !ok {
		return "", fmt.Errorf("input %q is not set", key)
	}
	switch v := v.(type) {
	case string:
		return v, nil
	case bool, float64, int, int64:
		return fmt.Sprintf("%v", v), nil
	default:
		return "", fmt.Errorf("input %q has unsupported type %T", key, v)
	}
}

func numericField(inputs map[string]interface{}, key string) (float64, error) {
	v, ok := inputs[key]
	if !ok {
		return 0, fmt.Errorf("input %q is not set", key)
	}
	switch n := v.(type) {
	case float64:
		return n, nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case string:
		f, err := strconv.ParseFloat(n, 64)
		if err != nil {
			return 0, fmt.Errorf("input %q = %q is not numeric: %w", key, n, err)
		}
		return f, nil
	default:
		return 0, fmt.Errorf("input %q has unsupported type %T", key, v)
	}
}

// AutomatedEvaluator implements invoke.Automated: it runs the named check for
// a gate's AutomatedGate.Check against the gate's flattened InvocationEnvelope
// Inputs (see the package-level convention above).
type AutomatedEvaluator struct {
	// Checks is the check registry, keyed by AutomatedGate.Check. Defaults to
	// DefaultChecks() when nil.
	Checks map[string]CheckFunc
}

// NewAutomatedEvaluator returns an AutomatedEvaluator over DefaultChecks.
func NewAutomatedEvaluator() *AutomatedEvaluator {
	return &AutomatedEvaluator{Checks: DefaultChecks()}
}

// Evaluate implements invoke.Automated.
func (e *AutomatedEvaluator) Evaluate(_ context.Context, gate apiv1.AutomatedGate, env apiv1.InvocationEnvelope) (string, error) {
	checks := e.Checks
	if checks == nil {
		checks = DefaultChecks()
	}
	check, ok := checks[gate.Check]
	if !ok {
		return "", fmt.Errorf("gate: unknown automated check %q", gate.Check)
	}
	return check(env.Inputs, gate.Params)
}

// EvaluateVerdict adapts Evaluate's outcome string into an apiv1.Verdict —
// for callers that need a uniform Verdict from every gate evaluator kind
// (e.g. the local runner's GateEvaluator seam, internal/runner#17, whose
// Evaluate returns apiv1.Verdict for both automated and agentic gates so the
// runner can branch on Decision uniformly). Only valid for checks that return
// OutcomePass/OutcomeFail (every check in DefaultChecks does); a custom check
// returning a different outcome string has no VerdictDecision to map to and
// should be driven through Evaluate directly against a seam that accepts a
// raw outcome (e.g. invoke.Automated), not through this method.
func (e *AutomatedEvaluator) EvaluateVerdict(ctx context.Context, gate apiv1.AutomatedGate, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	outcome, err := e.Evaluate(ctx, gate, env)
	if err != nil {
		return apiv1.Verdict{}, err
	}
	decision := apiv1.VerdictFail
	if outcome == OutcomePass {
		decision = apiv1.VerdictPass
	}
	return apiv1.Verdict{
		Decision: decision,
		Summary:  fmt.Sprintf("automated check %q: %s", gate.Check, outcome),
	}, nil
}
