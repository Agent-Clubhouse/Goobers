package creditgraph

import (
	"errors"
	"fmt"
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
// as fileable, and where it came from.
type FilingThreshold struct {
	Confidence        float64 `json:"confidence"`
	Source            string  `json:"source"`
	CalibrationTarget string  `json:"calibrationTarget,omitempty"`
	EvalSetVersion    string  `json:"evalSetVersion,omitempty"`
	EvalSetDigest     string  `json:"evalSetDigest,omitempty"`
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
	if err := validateEvalAccuracy(summary.Accuracy); err != nil {
		return err
	}
	if summary.Calibration.Target != FilingCalibrationTarget {
		return fmt.Errorf("attribution eval summary calibration target is %q, want %q",
			summary.Calibration.Target, FilingCalibrationTarget)
	}
	binned := 0
	for index, bin := range summary.Calibration.Bins {
		if bin.Lower < 0 || bin.Upper > 1 || bin.Lower >= bin.Upper || bin.Count < 0 || bin.Correct < 0 || bin.Correct > bin.Count {
			return fmt.Errorf("attribution eval summary calibration bin %d is malformed", index)
		}
		if index > 0 && bin.Lower < summary.Calibration.Bins[index-1].Upper {
			return fmt.Errorf("attribution eval summary calibration bin %d overlaps its predecessor", index)
		}
		binned += bin.Count
	}
	if binned != summary.CaseCount {
		return fmt.Errorf("attribution eval summary calibration bins hold %d cases, want %d", binned, summary.CaseCount)
	}
	return nil
}

// DeriveFilingThreshold is the auditor's filing confidence threshold given a
// published eval summary. Without one (nil) it is the default. Otherwise it is
// the lowest confidence above which every populated calibration bin measured
// at least FilingTargetPrecision accuracy: the upper bound of the highest
// populated bin that fell short, or the lower bound of the lowest populated
// bin when none did, but never below the default: the eval measures per-run
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
	threshold, populated := 1.0, false
	for index := len(summary.Calibration.Bins) - 1; index >= 0; index-- {
		bin := summary.Calibration.Bins[index]
		if bin.Count == 0 {
			continue
		}
		populated = true
		if float64(bin.Correct)/float64(bin.Count) < FilingTargetPrecision {
			threshold = bin.Upper
			break
		}
		threshold = bin.Lower
	}
	if !populated {
		return defaults
	}
	return FilingThreshold{
		Confidence:        round(max(threshold, DefaultFilingConfidenceThreshold)),
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
// named and with 0 <= correct <= total.
func validateEvalAccuracy(accuracy []AttributionEvalAccuracy) error {
	if len(accuracy) == 0 {
		return errors.New("attribution eval summary needs per-granularity accuracy")
	}
	for index, item := range accuracy {
		if strings.TrimSpace(item.Granularity) == "" || item.Correct < 0 || item.Correct > item.Total {
			return fmt.Errorf("attribution eval summary accuracy %d is malformed", index)
		}
	}
	return nil
}
