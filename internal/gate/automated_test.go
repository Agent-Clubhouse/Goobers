package gate

import (
	"context"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
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
	out, err := evalCheck(t, "ci-status", nil, map[string]interface{}{"ciStatus": "success"})
	if err != nil || out != OutcomePass {
		t.Fatalf("got %q, %v; want pass", out, err)
	}
	out, err = evalCheck(t, "ci-status", nil, map[string]interface{}{"ciStatus": "pending"})
	if err != nil || out != OutcomeFail {
		t.Fatalf("got %q, %v; want fail", out, err)
	}
}

func TestUnknownCheckErrors(t *testing.T) {
	if _, err := evalCheck(t, "no-such-check", nil, nil); err == nil {
		t.Fatal("want error for unknown check name")
	}
}
