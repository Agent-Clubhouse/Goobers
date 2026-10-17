package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/goobers/goobers/internal/creditgraph"
)

const (
	reportSchemaVersion = "goobers.dev/backprop/reviewer-eval-report/v1"
	// scoreTolerance absorbs float rounding when comparing two reports.
	scoreTolerance = 1e-9
)

// szzCalibration measures R-SZZ against the hand-labeled sample of the
// replayed split: how often the blamed commit is the one a human named.
type szzCalibration struct {
	Labeled   int     `json:"labeled"`
	Agreed    int     `json:"agreed"`
	Agreement float64 `json:"agreement"`
}

// evalReport is one replay of a frozen dataset by one reviewer variant.
// Cases are scored in sampled order, which is fixed by the seed, so a given
// dataset, seed, and reviewer always yield the same report.
type evalReport struct {
	Schema          string  `json:"schema"`
	DatasetVersion  string  `json:"datasetVersion"`
	DatasetDigest   string  `json:"datasetDigest"`
	Split           string  `json:"split"`
	Variant         string  `json:"variant"`
	Seed            string  `json:"seed"`
	MaxCases        int     `json:"maxCases"`
	BudgetUSD       float64 `json:"budgetUsd"`
	Sampled         int     `json:"sampled"`
	Reviewed        int     `json:"reviewed"`
	Failed          int     `json:"failed"`
	BudgetExhausted int     `json:"budgetExhausted"`
	OutOfDiff       int     `json:"outOfDiff"`
	// JudgeSkippedForBudget counts findings left unjudged, and so unmatched,
	// because the budget ran out.
	JudgeSkippedForBudget int                               `json:"judgeSkippedForBudget,omitempty"`
	Overall               creditgraph.ReviewerEvalMetrics   `json:"overall"`
	Classes               []creditgraph.ReviewerEvalMetrics `json:"classes"`
	TotalCostUSD          float64                           `json:"totalCostUsd"`
	CostPerBugUSD         float64                           `json:"costPerBugUsd"`
	SZZCalibration        szzCalibration                    `json:"szzCalibration"`
	Results               []caseResult                      `json:"results"`
}

func newReport(set dataset, options replayOptions, outOfDiff int, results []caseResult) evalReport {
	report := evalReport{
		Schema: reportSchemaVersion, DatasetVersion: set.Header.Version, DatasetDigest: set.Header.Digest,
		Split: options.Split, Variant: options.Variant, Seed: options.Seed, MaxCases: options.MaxCases,
		BudgetUSD: options.BudgetUSD, Sampled: len(results), OutOfDiff: outOfDiff,
		SZZCalibration: calibrateSZZ(set, options.Split), Results: results,
	}
	// Spend on failed cases counts toward the total, so cost per bug is what
	// each reviewed bug really cost.
	total := 0.0
	for _, result := range results {
		total += result.CostUSD
		for _, decision := range result.JudgeDecisions {
			if decision.SkippedForBudget {
				report.JudgeSkippedForBudget++
			}
		}
		switch result.Status {
		case statusReviewed:
			report.Reviewed++
		case statusFailed:
			report.Failed++
		case statusBudgetExhausted:
			report.BudgetExhausted++
		}
	}
	report.TotalCostUSD = round4(total)
	if report.Reviewed > 0 {
		report.CostPerBugUSD = round4(report.TotalCostUSD / float64(report.Reviewed))
	}
	report.Overall, report.Classes = scoreResults(results)
	return report
}

// scoreResults computes overall and per-class recall and precision over the
// reviewed cases. Per-class recall credits a bug to a class when a finding of
// that class matched it.
func scoreResults(results []caseResult) (creditgraph.ReviewerEvalMetrics, []creditgraph.ReviewerEvalMetrics) {
	overall := creditgraph.ReviewerEvalMetrics{}
	byClass := map[string]*creditgraph.ReviewerEvalMetrics{}
	for _, result := range results {
		if result.Status != statusReviewed {
			continue
		}
		overall.Evaluated++
		caughtBy := map[string]bool{}
		for _, finding := range result.Findings {
			metrics, ok := byClass[finding.Class]
			if !ok {
				metrics = &creditgraph.ReviewerEvalMetrics{Class: finding.Class}
				byClass[finding.Class] = metrics
			}
			overall.Findings++
			metrics.Findings++
			if finding.Match != "" {
				overall.MatchedFindings++
				metrics.MatchedFindings++
				caughtBy[finding.Class] = true
			}
		}
		for class := range caughtBy {
			byClass[class].Caught++
		}
		if result.Caught {
			overall.Caught++
		}
	}
	classes := make([]creditgraph.ReviewerEvalMetrics, 0, len(byClass))
	for _, metrics := range byClass {
		metrics.Evaluated = overall.Evaluated
		classes = append(classes, ratios(*metrics))
	}
	sort.Slice(classes, func(i, j int) bool { return classes[i].Class < classes[j].Class })
	return ratios(overall), classes
}

func ratios(metrics creditgraph.ReviewerEvalMetrics) creditgraph.ReviewerEvalMetrics {
	if metrics.Evaluated > 0 {
		metrics.Recall = round4(float64(metrics.Caught) / float64(metrics.Evaluated))
	}
	if metrics.Findings > 0 {
		metrics.Precision = round4(float64(metrics.MatchedFindings) / float64(metrics.Findings))
	}
	return metrics
}

func calibrateSZZ(set dataset, split string) szzCalibration {
	calibration := szzCalibration{}
	for _, item := range set.Cases {
		if item.Split != split || item.HandLabel == nil {
			continue
		}
		calibration.Labeled++
		if label := item.HandLabel.IntroducingCommit; label != "" && strings.HasPrefix(item.IntroducingCommit, label) {
			calibration.Agreed++
		}
	}
	if calibration.Labeled > 0 {
		calibration.Agreement = round4(float64(calibration.Agreed) / float64(calibration.Labeled))
	}
	return calibration
}

// summary is the report without per-case detail, in the form telemetry read
// surfaces publish.
func (report evalReport) summary() creditgraph.ReviewerEvalSummary {
	return creditgraph.ReviewerEvalSummary{
		Schema: creditgraph.ReviewerEvalSummarySchemaVersion, DatasetVersion: report.DatasetVersion,
		DatasetDigest: report.DatasetDigest, Split: report.Split, Variant: report.Variant,
		OutOfDiff: report.OutOfDiff, NotRun: report.Failed + report.BudgetExhausted,
		Overall: report.Overall, Classes: append([]creditgraph.ReviewerEvalMetrics{}, report.Classes...),
		TotalCostUSD: report.TotalCostUSD, CostPerBugUSD: report.CostPerBugUSD,
	}
}

// compare scores a candidate reviewer variant against the baseline. Both
// reports must have replayed the same frozen dataset and split and reviewed
// exactly the same cases; otherwise they are not comparable. A drop in
// overall recall or precision, or in any baseline class's recall, is a
// regression and blocks promotion.
func compare(baseline, candidate evalReport) (creditgraph.ReviewerEvalComparison, error) {
	if baseline.DatasetVersion != candidate.DatasetVersion || baseline.DatasetDigest != candidate.DatasetDigest {
		return creditgraph.ReviewerEvalComparison{}, fmt.Errorf("reports replayed different datasets (%s %s, %s %s)",
			baseline.DatasetVersion, baseline.DatasetDigest, candidate.DatasetVersion, candidate.DatasetDigest)
	}
	if baseline.Split != candidate.Split {
		return creditgraph.ReviewerEvalComparison{}, fmt.Errorf("reports replayed different splits (%s, %s)", baseline.Split, candidate.Split)
	}
	if baseline.Variant == candidate.Variant {
		return creditgraph.ReviewerEvalComparison{}, fmt.Errorf("baseline and candidate are both variant %q", baseline.Variant)
	}
	if !sameReviewedCases(baseline, candidate) {
		return creditgraph.ReviewerEvalComparison{}, errors.New("reports reviewed different cases; replay both with the same seed, case cap, and enough budget")
	}
	comparison := creditgraph.ReviewerEvalComparison{
		BaselineVariant: baseline.Variant, Baseline: baseline.Overall,
		BaselineClasses:       append([]creditgraph.ReviewerEvalMetrics{}, baseline.Classes...),
		BaselineCostPerBugUSD: baseline.CostPerBugUSD,
		RecallDelta:           round4(candidate.Overall.Recall - baseline.Overall.Recall),
		PrecisionDelta:        round4(candidate.Overall.Precision - baseline.Overall.Precision),
		CostPerBugDeltaUSD:    round4(candidate.CostPerBugUSD - baseline.CostPerBugUSD),
		Regressions:           []string{},
	}
	if candidate.Overall.Recall+scoreTolerance < baseline.Overall.Recall {
		comparison.Regressions = append(comparison.Regressions, fmt.Sprintf("recall %.4f below baseline %.4f", candidate.Overall.Recall, baseline.Overall.Recall))
	}
	if candidate.Overall.Precision+scoreTolerance < baseline.Overall.Precision {
		comparison.Regressions = append(comparison.Regressions, fmt.Sprintf("precision %.4f below baseline %.4f", candidate.Overall.Precision, baseline.Overall.Precision))
	}
	candidateRecall := map[string]float64{}
	for _, class := range candidate.Classes {
		candidateRecall[class.Class] = class.Recall
	}
	for _, class := range baseline.Classes {
		if candidateRecall[class.Class]+scoreTolerance < class.Recall {
			comparison.Regressions = append(comparison.Regressions, fmt.Sprintf("%s recall %.4f below baseline %.4f", class.Class, candidateRecall[class.Class], class.Recall))
		}
	}
	comparison.Promotable = len(comparison.Regressions) == 0
	return comparison, nil
}

func sameReviewedCases(baseline, candidate evalReport) bool {
	reviewed := func(report evalReport) []string {
		var ids []string
		for _, result := range report.Results {
			if result.Status == statusReviewed {
				ids = append(ids, result.CaseID)
			}
		}
		sort.Strings(ids)
		return ids
	}
	left, right := reviewed(baseline), reviewed(candidate)
	if len(left) != len(right) || len(left) == 0 {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
