package creditgraph

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// AttributionEvalSummarySchemaVersion identifies the published attribution
// eval summary contract (#7123).
const AttributionEvalSummarySchemaVersion = "goobers.dev/backprop/attribution-eval-summary/v1"

const (
	// DefaultFilingConfidenceThreshold is the minimum fault-finding confidence
	// the auditor treats as fileable when no attribution eval has been
	// published. It sits above every capped confidence (sparse, missing
	// provenance, contradictory, mixed or unknown domain), so uncalibrated
	// findings that the auditor itself withholds from one owner are never
	// fileable.
	DefaultFilingConfidenceThreshold = 0.5
	// FilingTargetPrecision is the accuracy every calibration bin at or above
	// a derived filing threshold must have measured.
	FilingTargetPrecision = 0.8
	// FilingCalibrationTarget is the granularity a published calibration must
	// be measured against: the auditor files findings by fault domain.
	FilingCalibrationTarget = "domain"
)

// Where a FilingThreshold came from.
const (
	// FilingThresholdDefault is DefaultFilingConfidenceThreshold, used when no
	// attribution eval with measured calibration has been published.
	FilingThresholdDefault = "default"
	// FilingThresholdAttributionEval is derived from a published eval's
	// measured calibration.
	FilingThresholdAttributionEval = "attribution-eval"
)

// AttributionEvalAccuracy is attribution accuracy at one granularity.
type AttributionEvalAccuracy struct {
	Granularity string  `json:"granularity"`
	Correct     int     `json:"correct"`
	Total       int     `json:"total"`
	Accuracy    float64 `json:"accuracy"`
}

// AttributionEvalBin summarizes eval predictions whose confidence fell in
// [Lower, Upper); the last bin includes 1.
type AttributionEvalBin struct {
	Lower          float64 `json:"lower"`
	Upper          float64 `json:"upper"`
	Count          int     `json:"count"`
	Correct        int     `json:"correct"`
	MeanConfidence float64 `json:"meanConfidence"`
	Accuracy       float64 `json:"accuracy"`
}

// AttributionEvalCalibration measures how well cause confidence predicts a
// correct attribution at Target granularity.
type AttributionEvalCalibration struct {
	Target                   string               `json:"target"`
	BrierScore               float64              `json:"brierScore"`
	ExpectedCalibrationError float64              `json:"expectedCalibrationError"`
	Bins                     []AttributionEvalBin `json:"bins"`
}

// AttributionEvalSummary is the published result of one attribution eval run:
// which labeled set was scored, per-granularity accuracy, and calibration. It
// omits per-case detail so read surfaces can carry it.
type AttributionEvalSummary struct {
	Schema         string                     `json:"schema"`
	EvalSetVersion string                     `json:"evalSetVersion"`
	EvalSetDigest  string                     `json:"evalSetDigest"`
	CaseCount      int                        `json:"caseCount"`
	Unattributed   int                        `json:"unattributed"`
	Accuracy       []AttributionEvalAccuracy  `json:"accuracy"`
	Calibration    AttributionEvalCalibration `json:"calibration"`
}

// FilingThreshold is the minimum fault-finding confidence the auditor treats
// as fileable, and where it came from. NoneFileable means the measured
// calibration fell short even in the bin that includes confidence 1, so no
// confidence is safe to file and Confidence is only informational.
type FilingThreshold struct {
	Confidence        float64 `json:"confidence"`
	NoneFileable      bool    `json:"noneFileable,omitempty"`
	Source            string  `json:"source"`
	CalibrationTarget string  `json:"calibrationTarget,omitempty"`
	EvalSetVersion    string  `json:"evalSetVersion,omitempty"`
	EvalSetDigest     string  `json:"evalSetDigest,omitempty"`
}

// Admits reports whether a finding at confidence is fileable.
func (threshold FilingThreshold) Admits(confidence float64) bool {
	return !threshold.NoneFileable && confidence >= threshold.Confidence
}

// Validate rejects a summary that is not a well-formed published eval result.
func (summary AttributionEvalSummary) Validate() error {
	if summary.Schema != AttributionEvalSummarySchemaVersion {
		return fmt.Errorf("unsupported attribution eval summary schema %q", summary.Schema)
	}
	if strings.TrimSpace(summary.EvalSetVersion) == "" || strings.TrimSpace(summary.EvalSetDigest) == "" {
		return errors.New("attribution eval summary needs an eval set version and digest")
	}
	if summary.CaseCount < 1 || summary.Unattributed < 0 || summary.Unattributed > summary.CaseCount {
		return fmt.Errorf("attribution eval summary case counts are inconsistent (%d cases, %d unattributed)",
			summary.CaseCount, summary.Unattributed)
	}
	if err := validateEvalAccuracy(summary.Accuracy, summary.CaseCount); err != nil {
		return err
	}
	if summary.Calibration.Target != FilingCalibrationTarget {
		return fmt.Errorf("attribution eval summary calibration target is %q, want %q",
			summary.Calibration.Target, FilingCalibrationTarget)
	}
	if !unitInterval(summary.Calibration.BrierScore) || !unitInterval(summary.Calibration.ExpectedCalibrationError) {
		return errors.New("attribution eval summary calibration scores must be within [0,1]")
	}
	return validateEvalBins(summary.Calibration.Bins, summary.CaseCount)
}

// validateEvalBins requires ordered, non-overlapping bins within [0,1] whose
// counts sum to caseCount, each with a mean confidence inside the bin and an
// accuracy that matches its counts.
func validateEvalBins(bins []AttributionEvalBin, caseCount int) error {
	binned := 0
	for index, bin := range bins {
		if bin.Lower < 0 || bin.Upper > 1 || bin.Lower >= bin.Upper || bin.Count < 0 || bin.Correct < 0 || bin.Correct > bin.Count {
			return fmt.Errorf("attribution eval summary calibration bin %d is malformed", index)
		}
		if index > 0 && bin.Lower < bins[index-1].Upper {
			return fmt.Errorf("attribution eval summary calibration bin %d overlaps its predecessor", index)
		}
		if !unitInterval(bin.MeanConfidence) || (bin.Count > 0 && (bin.MeanConfidence < bin.Lower || bin.MeanConfidence > bin.Upper)) {
			return fmt.Errorf("attribution eval summary calibration bin %d mean confidence is outside the bin", index)
		}
		if !ratioMatches(bin.Accuracy, bin.Correct, bin.Count) {
			return fmt.Errorf("attribution eval summary calibration bin %d accuracy does not match %d/%d", index, bin.Correct, bin.Count)
		}
		binned += bin.Count
	}
	if binned != caseCount {
		return fmt.Errorf("attribution eval summary calibration bins hold %d cases, want %d", binned, caseCount)
	}
	return nil
}

// evalRatioTolerance absorbs the eval report's rounding of ratios to four
// decimal places, plus float error so a ratio rounded from a tie (1/32 =
// 0.03125 to 0.0313) still matches.
const evalRatioTolerance = 5e-5 + 1e-9

// ratioMatches reports whether a published ratio is correct/total (0 when
// total is 0) to within the report's rounding.
func ratioMatches(ratio float64, correct, total int) bool {
	want := 0.0
	if total > 0 {
		want = float64(correct) / float64(total)
	}
	return unitInterval(ratio) && math.Abs(ratio-want) <= evalRatioTolerance
}

func unitInterval(value float64) bool {
	return value >= 0 && value <= 1
}

// DeriveFilingThreshold is the auditor's filing confidence threshold given a
// published eval summary. Without one (nil) it is the default. Otherwise it is
// the lowest confidence above which every populated calibration bin measured
// at least FilingTargetPrecision accuracy: the upper bound of the highest
// populated bin that fell short, or the lower bound of the lowest populated
// bin when none did, but never below the default. When the bin that includes
// confidence 1 fell short, no confidence is safe and the threshold is
// NoneFileable. The floor holds because the eval measures per-run
// attribution, while the auditor's own confidence caps (sparse samples,
// missing provenance, mixed domains) reflect cohort evidence it does not, so a
// measured calibration can only raise the bar. A calibration with no populated
// bin keeps the default.
// The result depends only on the summary, so it is deterministic.
func DeriveFilingThreshold(summary *AttributionEvalSummary) FilingThreshold {
	defaults := FilingThreshold{Confidence: DefaultFilingConfidenceThreshold, Source: FilingThresholdDefault}
	if summary == nil {
		return defaults
	}
	threshold, populated, short := 1.0, false, false
	for index := len(summary.Calibration.Bins) - 1; index >= 0; index-- {
		bin := summary.Calibration.Bins[index]
		if bin.Count == 0 {
			continue
		}
		populated = true
		if float64(bin.Correct)/float64(bin.Count) < FilingTargetPrecision {
			threshold, short = bin.Upper, true
			break
		}
		threshold = bin.Lower
	}
	if !populated {
		return defaults
	}
	return FilingThreshold{
		Confidence:        round(max(threshold, DefaultFilingConfidenceThreshold)),
		NoneFileable:      short && threshold >= 1,
		Source:            FilingThresholdAttributionEval,
		CalibrationTarget: summary.Calibration.Target,
		EvalSetVersion:    summary.EvalSetVersion,
		EvalSetDigest:     summary.EvalSetDigest,
	}
}

// normalizeFilingThreshold fills an unset threshold with the default.
func normalizeFilingThreshold(threshold FilingThreshold) FilingThreshold {
	if threshold.Source == "" {
		return DeriveFilingThreshold(nil)
	}
	return threshold
}

// validateEvalAccuracy requires at least one per-granularity result, each
// named, scored over every case, with 0 <= correct <= total and an accuracy
// that matches its counts.
func validateEvalAccuracy(accuracy []AttributionEvalAccuracy, caseCount int) error {
	if len(accuracy) == 0 {
		return errors.New("attribution eval summary needs per-granularity accuracy")
	}
	for index, item := range accuracy {
		if strings.TrimSpace(item.Granularity) == "" || item.Correct < 0 || item.Correct > item.Total {
			return fmt.Errorf("attribution eval summary accuracy %d is malformed", index)
		}
		if item.Total != caseCount {
			return fmt.Errorf("attribution eval summary accuracy %d scored %d cases, want %d", index, item.Total, caseCount)
		}
		if !ratioMatches(item.Accuracy, item.Correct, item.Total) {
			return fmt.Errorf("attribution eval summary accuracy %d does not match %d/%d", index, item.Correct, item.Total)
		}
	}
	return nil
}
