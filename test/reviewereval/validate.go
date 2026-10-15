package main

import (
	"errors"
	"fmt"
	"math"
	"reflect"

	"github.com/goobers/goobers/internal/creditgraph"
)

// roundingSlack is the most round4 moves one recorded cost. Replay enforces
// the budget on raw spend but records rounded costs, so checks of recorded
// costs against the budget or judge spend allow this much per rounded value.
const roundingSlack = 5e-5 + costEpsilon

// validate checks that a loaded report is internally consistent before
// compare or publish trusts it: its sampling metadata is well formed, its
// per-case results are coherent, and every aggregate (counts, recall,
// precision, costs) is exactly what its per-case results recompute to.
func (report evalReport) validate() error {
	if report.Schema != reportSchemaVersion {
		return fmt.Errorf("unsupported reviewer eval report schema %q", report.Schema)
	}
	options := replayOptions{
		Split: report.Split, Variant: report.Variant, MaxCases: report.MaxCases, BudgetUSD: report.BudgetUSD, Seed: report.Seed,
	}
	if err := options.validate(); err != nil {
		return err
	}
	if report.DatasetVersion == "" || report.DatasetDigest == "" {
		return errors.New("report does not name its dataset version and digest")
	}
	if report.Sampled != len(report.Results) || report.Sampled > report.MaxCases || report.OutOfDiff < 0 {
		return fmt.Errorf("report sampled %d cases with %d results under a cap of %d", report.Sampled, len(report.Results), report.MaxCases)
	}
	if err := validateResultOrder(report.Seed, report.Results); err != nil {
		return err
	}
	for _, result := range report.Results {
		if err := result.validate(); err != nil {
			return fmt.Errorf("case %s: %w", result.CaseID, err)
		}
	}
	return report.validateAggregates(options)
}

// validateResultOrder checks case IDs are unique and in seeded sampling
// order, and that once the budget ran out no later case was reviewed.
func validateResultOrder(seed string, results []caseResult) error {
	exhausted := false
	for index, result := range results {
		if result.CaseID == "" {
			return fmt.Errorf("result %d has no case ID", index)
		}
		if index > 0 {
			previous := results[index-1].CaseID
			if previous == result.CaseID {
				return fmt.Errorf("case %s appears twice", result.CaseID)
			}
			if sampleRank(seed, previous) > sampleRank(seed, result.CaseID) {
				return fmt.Errorf("case %s is out of seeded sampling order", result.CaseID)
			}
		}
		if exhausted && result.Status != statusBudgetExhausted {
			return fmt.Errorf("case %s ran after the budget was exhausted", result.CaseID)
		}
		exhausted = result.Status == statusBudgetExhausted
	}
	return nil
}

func (result caseResult) validate() error {
	if !validCost(result.CostUSD) || !validCost(result.JudgeCostUSD) || result.JudgeCostUSD > result.CostUSD+roundingSlack {
		return fmt.Errorf("invalid cost %v (judge %v)", result.CostUSD, result.JudgeCostUSD)
	}
	judgeCost := 0.0
	for _, decision := range result.JudgeDecisions {
		judgeCost += decision.CostUSD
	}
	if math.Abs(judgeCost-result.JudgeCostUSD) > roundingSlack {
		return fmt.Errorf("judge cost %v does not match its decisions' %v", result.JudgeCostUSD, judgeCost)
	}
	switch result.Status {
	case statusReviewed:
		return result.validateReviewed()
	case statusFailed:
		if result.Error == "" || len(result.Findings) > 0 || result.Caught {
			return errors.New("a failed case must record its error and no scored findings")
		}
	case statusBudgetExhausted:
		if result.CostUSD != 0 || result.Verdict != nil || len(result.Findings) > 0 || len(result.JudgeDecisions) > 0 || result.Caught {
			return errors.New("a budget-exhausted case must be empty")
		}
	default:
		return fmt.Errorf("unknown status %q", result.Status)
	}
	return nil
}

// validateReviewed checks a reviewed case's scoring against its verdict:
// one scored finding per verdict finding, judge matches backed by a recorded
// judge decision, and Caught exactly when some finding matched.
func (result caseResult) validateReviewed() error {
	if result.Verdict == nil || len(result.Findings) != len(result.Verdict.Findings) {
		return errors.New("a reviewed case must record its verdict and score every finding")
	}
	judgedMatch := map[int]bool{}
	for _, decision := range result.JudgeDecisions {
		if decision.Finding < 0 || decision.Finding >= len(result.Findings) {
			return fmt.Errorf("judge decision names finding %d of %d", decision.Finding, len(result.Findings))
		}
		judgedMatch[decision.Finding] = decision.Match && !decision.SkippedForBudget
	}
	caught := false
	for index, finding := range result.Findings {
		switch finding.Match {
		case "", matchLineOverlap:
		case matchJudge:
			if !judgedMatch[index] {
				return fmt.Errorf("finding %d is a judge match with no matching judge decision", index)
			}
		default:
			return fmt.Errorf("finding %d has unknown match %q", index, finding.Match)
		}
		caught = caught || finding.Match != ""
	}
	if caught != result.Caught {
		return fmt.Errorf("caught is %v but its findings say %v", result.Caught, caught)
	}
	return nil
}

// validateAggregates recomputes every aggregate from the per-case results and
// requires the report to match it exactly.
func (report evalReport) validateAggregates(options replayOptions) error {
	set := dataset{Header: datasetHeader{Version: report.DatasetVersion, Digest: report.DatasetDigest}}
	want := newReport(set, options, report.OutOfDiff, report.Results)
	type counts struct{ reviewed, failed, exhausted, skipped int }
	if (counts{report.Reviewed, report.Failed, report.BudgetExhausted, report.JudgeSkippedForBudget}) !=
		(counts{want.Reviewed, want.Failed, want.BudgetExhausted, want.JudgeSkippedForBudget}) {
		return errors.New("report case counts do not match its results")
	}
	if report.Overall != want.Overall || !sameMetrics(report.Classes, want.Classes) {
		return errors.New("report recall or precision does not match its results")
	}
	if report.TotalCostUSD != want.TotalCostUSD || report.CostPerBugUSD != want.CostPerBugUSD {
		return fmt.Errorf("report cost %v (%v per bug) does not match its results' %v (%v per bug)",
			report.TotalCostUSD, report.CostPerBugUSD, want.TotalCostUSD, want.CostPerBugUSD)
	}
	if report.TotalCostUSD > report.BudgetUSD+roundingSlack*float64(len(report.Results)+1) {
		return fmt.Errorf("report spent %v over its %v budget", report.TotalCostUSD, report.BudgetUSD)
	}
	calibration := report.SZZCalibration
	if calibration.Agreed < 0 || calibration.Agreed > calibration.Labeled ||
		calibration.Agreement != ratios(creditgraph.ReviewerEvalMetrics{Caught: calibration.Agreed, Evaluated: calibration.Labeled}).Recall {
		return fmt.Errorf("invalid SZZ calibration %+v", calibration)
	}
	return nil
}

func sameMetrics(left, right []creditgraph.ReviewerEvalMetrics) bool {
	if len(left) == 0 && len(right) == 0 {
		return true
	}
	return reflect.DeepEqual(left, right)
}
