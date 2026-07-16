package gate

import (
	"context"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	wf "github.com/goobers/goobers/internal/workflow"
)

func evalCheck(t *testing.T, check string, params map[string]string, inputs map[string]interface{}) (string, error) {
	t.Helper()
	e := NewAutomatedEvaluator()
	env := apiv1.InvocationEnvelope{Inputs: inputs}
	return e.Evaluate(context.Background(), apiv1.AutomatedGate{Check: check, Params: params}, env)
}

func TestStatusEqualsDefaultsToSuccess(t *testing.T) {
	out, err := evalCheck(t, "status-equals", nil, map[string]interface{}{InputKeyStatus: "success"})
	if err != nil || out != OutcomePass {
		t.Fatalf("got %q, %v; want pass", out, err)
	}
	out, err = evalCheck(t, "status-equals", nil, map[string]interface{}{InputKeyStatus: "failure"})
	if err != nil || out != OutcomeFail {
		t.Fatalf("got %q, %v; want fail", out, err)
	}
}

func TestStatusEqualsCustomTarget(t *testing.T) {
	out, err := evalCheck(t, "status-equals", map[string]string{"equals": "blocked"}, map[string]interface{}{InputKeyStatus: "blocked"})
	if err != nil || out != OutcomePass {
		t.Fatalf("got %q, %v; want pass", out, err)
	}
}

func TestOutputEqualsRequiresParams(t *testing.T) {
	if _, err := evalCheck(t, "output-equals", nil, nil); err == nil {
		t.Fatal("want error for missing params.key/equals")
	}
	if _, err := evalCheck(t, "output-equals", map[string]string{"key": "k"}, nil); err == nil {
		t.Fatal("want error for missing params.equals")
	}
}

func TestOutputEquals(t *testing.T) {
	out, err := evalCheck(t, "output-equals", map[string]string{"key": "branch", "equals": "main"}, map[string]interface{}{"branch": "main"})
	if err != nil || out != OutcomePass {
		t.Fatalf("got %q, %v; want pass", out, err)
	}
	out, err = evalCheck(t, "output-equals", map[string]string{"key": "branch", "equals": "main"}, map[string]interface{}{"branch": "dev"})
	if err != nil || out != OutcomeFail {
		t.Fatalf("got %q, %v; want fail", out, err)
	}
}

func TestOutputNumericGTE(t *testing.T) {
	cases := []struct {
		name      string
		value     interface{}
		threshold string
		want      string
	}{
		{"float above", 85.5, "80", OutcomePass},
		{"float below", 70.0, "80", OutcomeFail},
		{"int equal", 80, "80", OutcomePass},
		{"string numeric", "81", "80", OutcomePass},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := evalCheck(t, "output-numeric-gte", map[string]string{"key": "coverage", "threshold": tc.threshold}, map[string]interface{}{"coverage": tc.value})
			if err != nil || out != tc.want {
				t.Fatalf("got %q, %v; want %q", out, err, tc.want)
			}
		})
	}
}

func TestOutputNumericGTEErrorsOnBadInput(t *testing.T) {
	if _, err := evalCheck(t, "output-numeric-gte", map[string]string{"key": "coverage", "threshold": "not-a-number"}, map[string]interface{}{"coverage": 80.0}); err == nil {
		t.Fatal("want error for non-numeric threshold")
	}
	if _, err := evalCheck(t, "output-numeric-gte", map[string]string{"key": "coverage", "threshold": "80"}, map[string]interface{}{"coverage": "nope"}); err == nil {
		t.Fatal("want error for non-numeric input value")
	}
	if _, err := evalCheck(t, "output-numeric-gte", map[string]string{"key": "coverage", "threshold": "80"}, nil); err == nil {
		t.Fatal("want error for missing input value")
	}
}

func TestCIStatusCheck(t *testing.T) {
	// Default vocabulary is providers.CheckState ("passing"/"failing"/
	// "pending" — internal/gate does not import providers, see
	// checkEqualsVocab's doc comment), not apiv1.ResultStatus's "success"
	// (#132: a ci-poll stage emits "passing"/"failing", so the check must
	// default to matching that, not the unrelated ResultStatus vocabulary).
	out, err := evalCheck(t, "ci-status", nil, map[string]interface{}{"ciStatus": "passing"})
	if err != nil || out != OutcomePass {
		t.Fatalf("got %q, %v; want pass", out, err)
	}
	out, err = evalCheck(t, "ci-status", nil, map[string]interface{}{"ciStatus": "pending"})
	if err != nil || out != OutcomeFail {
		t.Fatalf("got %q, %v; want fail", out, err)
	}
}

// TestCIStatusCheckTimeoutIsADistinctOutcome is the routing regression test
// for #239: a ci-poll timeout (executor.CIStatusTimeout) must resolve to its
// own OutcomeTimeout, not get folded into OutcomeFail — a workflow's ci-gate
// needs to tell "CI ran and failed" apart from "the poll never reached a
// terminal state" so it can route the latter to escalation instead of an
// implement repass.
func TestCIStatusCheckTimeoutIsADistinctOutcome(t *testing.T) {
	out, err := evalCheck(t, "ci-status", nil, map[string]interface{}{"ciStatus": executor.CIStatusTimeout})
	if err != nil || out != OutcomeTimeout {
		t.Fatalf("got %q, %v; want %q", out, err, OutcomeTimeout)
	}
	if out == OutcomeFail {
		t.Fatal("a poll timeout must not resolve to the same outcome as a genuine CI failure")
	}
}

// TestCIGateRoutesTimeoutOutcomeToEscalate proves the full gate.Evaluate path
// (not just the check function) resolves a ci-poll timeout to @escalate when
// a workflow's ci-gate declares a "timeout" branch — the routing half of
// #239, exercised through the same Evaluator a real run uses.
func TestCIGateRoutesTimeoutOutcomeToEscalate(t *testing.T) {
	g := apiv1.Gate{
		Name:      "ci-gate",
		Evaluator: apiv1.EvaluatorAutomated,
		Automated: &apiv1.AutomatedGate{Check: "ci-status"},
		Branches: map[string]string{
			OutcomePass:    "close-out",
			OutcomeFail:    "implement",
			OutcomeTimeout: wf.TargetEscalate,
		},
	}
	e := &Evaluator{Automated: NewAutomatedEvaluator()}
	env := apiv1.InvocationEnvelope{Inputs: map[string]interface{}{"ciStatus": executor.CIStatusTimeout}}

	result, err := e.Evaluate(context.Background(), g, env, "ci-poll", apiv1.ResultEnvelope{Status: apiv1.ResultFailure}, "", false)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if result.Outcome != OutcomeTimeout {
		t.Fatalf("outcome = %q, want %q", result.Outcome, OutcomeTimeout)
	}
	if result.Target != wf.TargetEscalate {
		t.Fatalf("target = %q, want %q (not the implement repass)", result.Target, wf.TargetEscalate)
	}
}

func TestUnknownCheckErrors(t *testing.T) {
	if _, err := evalCheck(t, "no-such-check", nil, nil); err == nil {
		t.Fatal("want error for unknown check name")
	}
}

func TestEvaluateVerdictMapsOutcomeToDecision(t *testing.T) {
	e := NewAutomatedEvaluator()
	env := apiv1.InvocationEnvelope{Inputs: map[string]interface{}{InputKeyStatus: "success"}}
	v, err := e.EvaluateVerdict(context.Background(), apiv1.AutomatedGate{Check: "status-equals"}, env)
	if err != nil || v.Decision != apiv1.VerdictPass {
		t.Fatalf("got %+v, %v; want VerdictPass", v, err)
	}

	env.Inputs[InputKeyStatus] = "failure"
	v, err = e.EvaluateVerdict(context.Background(), apiv1.AutomatedGate{Check: "status-equals"}, env)
	if err != nil || v.Decision != apiv1.VerdictFail {
		t.Fatalf("got %+v, %v; want VerdictFail", v, err)
	}
}

func TestEvaluateVerdictPropagatesError(t *testing.T) {
	e := NewAutomatedEvaluator()
	if _, err := e.EvaluateVerdict(context.Background(), apiv1.AutomatedGate{Check: "no-such-check"}, apiv1.InvocationEnvelope{}); err == nil {
		t.Fatal("want error propagated from Evaluate")
	}
}
