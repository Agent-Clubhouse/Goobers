package goobernetes

import "testing"

func TestClassifyItem(t *testing.T) {
	cases := []struct {
		name         string
		precondition PreconditionFailure
		observed     bool
		want         Verdict
	}{
		{"pass", "", true, VerdictPass},
		{"fail", "", false, VerdictFail},
		{"invalid overrides observed=true", "journal unreadable", true, VerdictInvalid},
		{"invalid overrides observed=false", "SSE capture lost", false, VerdictInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := ClassifyItem(tc.precondition, tc.observed)
			if got != tc.want {
				t.Fatalf("ClassifyItem(%q, %v) = %v, want %v", tc.precondition, tc.observed, got, tc.want)
			}
			if tc.precondition != "" && reason != string(tc.precondition) {
				t.Fatalf("reason = %q, want precondition text %q", reason, tc.precondition)
			}
			if tc.precondition == "" && reason != "" {
				t.Fatalf("reason = %q, want empty when no precondition failure (ClassifyItem never explains a fail — callers supply their own detail)", reason)
			}
		})
	}
}

// TestClassifyItemNeverMasksAGenuineFail is the StagePopulation nil-vs-empty
// discipline this classifier explicitly inherits (verdict.go doc comment):
// a real negative result observed successfully must stay FAIL, never get
// reclassified as invalid just because the precondition string is empty.
func TestClassifyItemNeverMasksAGenuineFail(t *testing.T) {
	got, _ := ClassifyItem("", false)
	if got != VerdictFail {
		t.Fatalf("ClassifyItem(\"\", false) = %v, want %v (a genuine negative must never become invalid)", got, VerdictFail)
	}
}

func TestOverallVerdict(t *testing.T) {
	cases := []struct {
		name  string
		items []Verdict
		want  Verdict
	}{
		{"empty is invalid — a bundle that asserts nothing proves nothing", nil, VerdictInvalid},
		{"all pass", []Verdict{VerdictPass, VerdictPass, VerdictPass}, VerdictPass},
		{"one fail dominates over other passes", []Verdict{VerdictPass, VerdictFail, VerdictPass}, VerdictFail},
		{"one invalid dominates over fail and pass", []Verdict{VerdictPass, VerdictFail, VerdictInvalid}, VerdictInvalid},
		{"invalid dominates even with no fails", []Verdict{VerdictPass, VerdictInvalid}, VerdictInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := OverallVerdict(tc.items); got != tc.want {
				t.Fatalf("OverallVerdict(%v) = %v, want %v", tc.items, got, tc.want)
			}
		})
	}
}
