package creditgraph

import (
	"strings"
	"testing"
)

func validReviewerEvalSummary() ReviewerEvalSummary {
	return ReviewerEvalSummary{
		Schema: ReviewerEvalSummarySchemaVersion, DatasetVersion: "v1", DatasetDigest: "sha256:abc",
		Split: ReviewerEvalSplit, Variant: "candidate", OutOfDiff: 1, NotRun: 1,
		Overall: ReviewerEvalMetrics{Caught: 2, Evaluated: 3, Recall: 0.6667, MatchedFindings: 2, Findings: 4, Precision: 0.5},
		Classes: []ReviewerEvalMetrics{
			{Class: "missing-tests", Evaluated: 3, Findings: 1},
			{Class: "substantive", Caught: 2, Evaluated: 3, Recall: 0.6667, MatchedFindings: 2, Findings: 3, Precision: 0.6667},
		},
		TotalCostUSD: 1, CostPerBugUSD: 0.3333,
	}
}

func TestReviewerEvalSummaryValidate(t *testing.T) {
	if err := validReviewerEvalSummary().Validate(); err != nil {
		t.Fatalf("valid summary: %v", err)
	}
	withComparison := validReviewerEvalSummary()
	withComparison.Comparison = &ReviewerEvalComparison{
		BaselineVariant: "baseline",
		Baseline:        ReviewerEvalMetrics{Caught: 1, Evaluated: 3, Recall: 0.3333, MatchedFindings: 1, Findings: 1, Precision: 1},
		BaselineClasses: []ReviewerEvalMetrics{{Class: "substantive", Caught: 1, Evaluated: 3, Recall: 0.3333, MatchedFindings: 1, Findings: 1, Precision: 1}},
		RecallDelta:     0.3334, PrecisionDelta: -0.5, CostPerBugDeltaUSD: 0.3333,
		Regressions: []string{"precision 0.5000 below baseline 1.0000"},
	}
	if err := withComparison.Validate(); err != nil {
		t.Fatalf("summary with comparison: %v", err)
	}

	cases := map[string]func(*ReviewerEvalSummary){
		"training split":       func(s *ReviewerEvalSummary) { s.Split = "train" },
		"nothing evaluated":    func(s *ReviewerEvalSummary) { s.Overall = ReviewerEvalMetrics{}; s.Classes = nil },
		"recall off its count": func(s *ReviewerEvalSummary) { s.Overall.Recall = 0.9 },
		"class findings drift": func(s *ReviewerEvalSummary) { s.Classes[0].Findings = 2 },
		"unsorted classes":     func(s *ReviewerEvalSummary) { s.Classes[0], s.Classes[1] = s.Classes[1], s.Classes[0] },
		"cost per bug off":     func(s *ReviewerEvalSummary) { s.CostPerBugUSD = 1 },
		"promotable despite regression": func(s *ReviewerEvalSummary) {
			comparison := *withComparison.Comparison
			comparison.Promotable = true
			s.Comparison = &comparison
		},
		"compared with itself": func(s *ReviewerEvalSummary) {
			comparison := *withComparison.Comparison
			comparison.BaselineVariant = s.Variant
			s.Comparison = &comparison
		},
		"delta off its scores": func(s *ReviewerEvalSummary) {
			comparison := *withComparison.Comparison
			comparison.RecallDelta = 0
			s.Comparison = &comparison
		},
	}
	for name, mutate := range cases {
		summary := validReviewerEvalSummary()
		summary.Classes = append([]ReviewerEvalMetrics(nil), summary.Classes...)
		mutate(&summary)
		if err := summary.Validate(); err == nil {
			t.Errorf("%s: Validate() accepted the summary", name)
		} else if name == "training split" && !strings.Contains(err.Error(), "only the \"eval\" split") {
			t.Errorf("training split error = %v", err)
		}
	}
}
