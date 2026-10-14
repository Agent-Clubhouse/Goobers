package creditgraph

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// ReviewerEvalSummarySchemaVersion identifies the published reviewer
// recall/precision eval summary contract (#7112).
const ReviewerEvalSummarySchemaVersion = "goobers.dev/backprop/reviewer-eval-summary/v1"

// ReviewerEvalSplit is the only dataset split a published reviewer eval may
// describe: training cases tune reviewers and never score them.
const ReviewerEvalSplit = "eval"

// ReviewerEvalMetrics is merge-review recall and precision over one replayed
// case set, overall (Class empty) or for one finding class. Recall is
// Caught/Evaluated: bugs a finding matched over bugs evaluated. Precision is
// MatchedFindings/Findings: findings that matched their bug over all findings.
type ReviewerEvalMetrics struct {
	Class           string  `json:"class,omitempty"`
	Caught          int     `json:"caught"`
	Evaluated       int     `json:"evaluated"`
	Recall          float64 `json:"recall"`
	MatchedFindings int     `json:"matchedFindings"`
	Findings        int     `json:"findings"`
	Precision       float64 `json:"precision"`
}

// ReviewerEvalComparison sets a candidate reviewer variant's scores beside
// the baseline variant's on the same frozen dataset, split, and sampled
// cases. Promotable is true only when Regressions is empty.
type ReviewerEvalComparison struct {
	BaselineVariant       string                `json:"baselineVariant"`
	Baseline              ReviewerEvalMetrics   `json:"baseline"`
	BaselineClasses       []ReviewerEvalMetrics `json:"baselineClasses"`
	BaselineCostPerBugUSD float64               `json:"baselineCostPerBugUsd"`
	RecallDelta           float64               `json:"recallDelta"`
	PrecisionDelta        float64               `json:"precisionDelta"`
	CostPerBugDeltaUSD    float64               `json:"costPerBugDeltaUsd"`
	Regressions           []string              `json:"regressions"`
	Promotable            bool                  `json:"promotable"`
}

// ReviewerEvalSummary is the published result of one reviewer eval replay:
// which frozen dataset and split were scored, by which reviewer variant, with
// overall and per-class recall and precision and the cost per evaluated bug.
// OutOfDiff counts sampled bugs whose cause lay outside the offending diff;
// they are excluded from Evaluated rather than counted as misses. NotRun
// counts sampled cases a budget cap or replay failure left unscored.
type ReviewerEvalSummary struct {
	Schema         string                  `json:"schema"`
	DatasetVersion string                  `json:"datasetVersion"`
	DatasetDigest  string                  `json:"datasetDigest"`
	Split          string                  `json:"split"`
	Variant        string                  `json:"variant"`
	OutOfDiff      int                     `json:"outOfDiff"`
	NotRun         int                     `json:"notRun"`
	Overall        ReviewerEvalMetrics     `json:"overall"`
	Classes        []ReviewerEvalMetrics   `json:"classes"`
	TotalCostUSD   float64                 `json:"totalCostUsd"`
	CostPerBugUSD  float64                 `json:"costPerBugUsd"`
	Comparison     *ReviewerEvalComparison `json:"comparison,omitempty"`
}

// reviewerEvalCostTolerance absorbs the report's rounding of costs to four
// decimal places.
const reviewerEvalCostTolerance = 5e-5 + 1e-9

// Validate rejects a summary that is not a well-formed published reviewer
// eval: a training split, inconsistent counts, ratios or costs that do not
// match their counts, or a comparison against itself.
func (summary ReviewerEvalSummary) Validate() error {
	if summary.Schema != ReviewerEvalSummarySchemaVersion {
		return fmt.Errorf("unsupported reviewer eval summary schema %q", summary.Schema)
	}
	if strings.TrimSpace(summary.DatasetVersion) == "" || strings.TrimSpace(summary.DatasetDigest) == "" {
		return errors.New("reviewer eval summary needs a dataset version and digest")
	}
	if summary.Split != ReviewerEvalSplit {
		return fmt.Errorf("reviewer eval summary scores split %q; only the %q split may be published", summary.Split, ReviewerEvalSplit)
	}
	if strings.TrimSpace(summary.Variant) == "" {
		return errors.New("reviewer eval summary needs a reviewer variant")
	}
	if summary.OutOfDiff < 0 || summary.NotRun < 0 {
		return errors.New("reviewer eval summary case counts must not be negative")
	}
	if err := validateReviewerEvalScores(summary.Overall, summary.Classes); err != nil {
		return err
	}
	if !validCost(summary.TotalCostUSD) || !validCost(summary.CostPerBugUSD) ||
		math.Abs(summary.CostPerBugUSD-summary.TotalCostUSD/float64(summary.Overall.Evaluated)) > reviewerEvalCostTolerance {
		return errors.New("reviewer eval summary cost per bug does not match its total cost")
	}
	if summary.Comparison != nil {
		return summary.Comparison.validate(summary)
	}
	return nil
}

func validateReviewerEvalScores(overall ReviewerEvalMetrics, classes []ReviewerEvalMetrics) error {
	if overall.Class != "" {
		return errors.New("reviewer eval summary overall metrics must not name a class")
	}
	if overall.Evaluated < 1 {
		return errors.New("reviewer eval summary evaluated no bugs")
	}
	if err := overall.validate(); err != nil {
		return err
	}
	findings := 0
	for index, class := range classes {
		if strings.TrimSpace(class.Class) == "" {
			return fmt.Errorf("reviewer eval summary class %d has no name", index)
		}
		if index > 0 && class.Class <= classes[index-1].Class {
			return errors.New("reviewer eval summary classes must be unique and sorted")
		}
		if class.Evaluated != overall.Evaluated || class.Caught > overall.Caught || class.MatchedFindings > overall.MatchedFindings {
			return fmt.Errorf("reviewer eval summary class %q exceeds the overall counts", class.Class)
		}
		if err := class.validate(); err != nil {
			return err
		}
		findings += class.Findings
	}
	if findings != overall.Findings {
		return fmt.Errorf("reviewer eval summary classes hold %d findings, want %d", findings, overall.Findings)
	}
	return nil
}

func (metrics ReviewerEvalMetrics) validate() error {
	if metrics.Caught < 0 || metrics.Caught > metrics.Evaluated || metrics.MatchedFindings < 0 || metrics.MatchedFindings > metrics.Findings {
		return fmt.Errorf("reviewer eval metrics %q counts are inconsistent", metrics.Class)
	}
	if metrics.MatchedFindings > 0 && metrics.Caught == 0 {
		return fmt.Errorf("reviewer eval metrics %q match findings without catching a bug", metrics.Class)
	}
	if !ratioMatches(metrics.Recall, metrics.Caught, metrics.Evaluated) || !ratioMatches(metrics.Precision, metrics.MatchedFindings, metrics.Findings) {
		return fmt.Errorf("reviewer eval metrics %q ratios do not match their counts", metrics.Class)
	}
	return nil
}

func (comparison ReviewerEvalComparison) validate(candidate ReviewerEvalSummary) error {
	if strings.TrimSpace(comparison.BaselineVariant) == "" || comparison.BaselineVariant == candidate.Variant {
		return errors.New("reviewer eval comparison needs a baseline variant other than the candidate")
	}
	if comparison.Baseline.Evaluated != candidate.Overall.Evaluated {
		return errors.New("reviewer eval comparison baseline scored a different case set")
	}
	if err := validateReviewerEvalScores(comparison.Baseline, comparison.BaselineClasses); err != nil {
		return fmt.Errorf("reviewer eval comparison baseline: %w", err)
	}
	if !validCost(comparison.BaselineCostPerBugUSD) ||
		!deltaMatches(comparison.RecallDelta, candidate.Overall.Recall, comparison.Baseline.Recall) ||
		!deltaMatches(comparison.PrecisionDelta, candidate.Overall.Precision, comparison.Baseline.Precision) ||
		!deltaMatches(comparison.CostPerBugDeltaUSD, candidate.CostPerBugUSD, comparison.BaselineCostPerBugUSD) {
		return errors.New("reviewer eval comparison deltas do not match the scores they compare")
	}
	if comparison.Promotable != (len(comparison.Regressions) == 0) {
		return errors.New("reviewer eval comparison is promotable only without regressions")
	}
	return nil
}

func deltaMatches(delta, candidate, baseline float64) bool {
	return math.Abs(delta-(candidate-baseline)) <= 2*reviewerEvalCostTolerance
}

func validCost(value float64) bool {
	return value >= 0 && !math.IsInf(value, 0) && !math.IsNaN(value)
}
