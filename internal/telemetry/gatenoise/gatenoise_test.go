package gatenoise

import "testing"

func TestEvaluateMatchesTheLocalGateRules(t *testing.T) {
	for _, test := range []struct {
		name   string
		totals Totals
		min    int
		max    float64
		want   Verdict
	}{
		{"too few evaluations", Totals{Total: 4}, 5, 0.2, Verdict{}},
		{"never fails", Totals{Total: 5}, 5, 0.2, Verdict{NeverFails: true}},
		{"churn only", Totals{Total: 10, NonPass: 5, Escalated: 5}, 5, 0.2, Verdict{Churn: true, EscalationRate: 0.5}},
		{"both", Totals{Total: 5, Escalated: 2}, 5, 0.2, Verdict{NeverFails: true, Churn: true, EscalationRate: 0.4}},
		{"below the escalation threshold", Totals{Total: 10, NonPass: 1, Escalated: 1}, 5, 0.2, Verdict{EscalationRate: 0.1}},
		{"no evaluations", Totals{}, 0, 0, Verdict{}},
	} {
		if got := Evaluate(test.totals, test.min, test.max); got != test.want {
			t.Errorf("%s: Evaluate = %+v, want %+v", test.name, got, test.want)
		}
	}
}

// The reason sources report totals: a finding from source A alone (5 passes)
// says "never fails", but the summed totals with source B's escalated
// non-passes do not.
func TestSummedTotalsOverrideASingleSourcesNeverFails(t *testing.T) {
	a, b := Totals{Total: 5}, Totals{Total: 5, NonPass: 5, Escalated: 5}
	if !Evaluate(a, 5, 0.2).NeverFails {
		t.Fatal("premise: source A alone never fails")
	}
	merged := Evaluate(a.Add(b), 5, 0.2)
	if merged.NeverFails || !merged.Churn || merged.EscalationRate != 0.5 {
		t.Fatalf("merged = %+v, want churn at 0.5 and no never-fails", merged)
	}
}
