package creditgraph

import (
	"testing"
)

func calibrationSummary(bins ...AttributionEvalBin) *AttributionEvalSummary {
	summary := &AttributionEvalSummary{
		Schema: AttributionEvalSummarySchemaVersion, EvalSetVersion: "v1", EvalSetDigest: "sha256:abc",
		Calibration: AttributionEvalCalibration{Target: "domain", Bins: bins},
	}
	for _, bin := range bins {
		summary.CaseCount += bin.Count
	}
	return summary
}

func evalBin(lower, upper float64, count, correct int) AttributionEvalBin {
	return AttributionEvalBin{Lower: lower, Upper: upper, Count: count, Correct: correct}
}

func TestDeriveFilingThreshold(t *testing.T) {
	for _, tc := range []struct {
		name    string
		summary *AttributionEvalSummary
		want    float64
		source  string
	}{
		{name: "no eval keeps the default", summary: nil, want: DefaultFilingConfidenceThreshold, source: FilingThresholdDefault},
		{name: "no populated bin keeps the default", summary: calibrationSummary(evalBin(0, 0.5, 0, 0), evalBin(0.5, 1, 0, 0)),
			want: DefaultFilingConfidenceThreshold, source: FilingThresholdDefault},
		{name: "highest imprecise bin sets the floor at its upper bound",
			summary: calibrationSummary(evalBin(0.2, 0.4, 5, 3), evalBin(0.4, 0.6, 7, 5), evalBin(0.6, 0.8, 0, 0)),
			want:    0.6, source: FilingThresholdAttributionEval},
		{name: "precise bins above an imprecise one admit only the precise range",
			summary: calibrationSummary(evalBin(0.4, 0.6, 4, 1), evalBin(0.6, 0.8, 5, 4), evalBin(0.8, 1, 5, 5)),
			want:    0.6, source: FilingThresholdAttributionEval},
		{name: "an imprecise top bin admits nothing below certainty",
			summary: calibrationSummary(evalBin(0.6, 0.8, 5, 5), evalBin(0.8, 1, 5, 2)),
			want:    1, source: FilingThresholdAttributionEval},
		{name: "a fully precise calibration never lowers the bar below the default",
			summary: calibrationSummary(evalBin(0.2, 0.4, 5, 5), evalBin(0.4, 0.6, 5, 4)),
			want:    DefaultFilingConfidenceThreshold, source: FilingThresholdAttributionEval},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveFilingThreshold(tc.summary)
			if got.Confidence != tc.want || got.Source != tc.source {
				t.Fatalf("threshold = %+v, want confidence %v from %s", got, tc.want, tc.source)
			}
			if tc.source == FilingThresholdAttributionEval &&
				(got.EvalSetVersion != "v1" || got.EvalSetDigest != "sha256:abc" || got.CalibrationTarget != "domain") {
				t.Fatalf("threshold = %+v, want the eval set it was derived from", got)
			}
			if again := DeriveFilingThreshold(tc.summary); again != got {
				t.Fatalf("threshold is not deterministic: %+v then %+v", got, again)
			}
		})
	}
}

func TestAttributionEvalSummaryValidate(t *testing.T) {
	valid := calibrationSummary(evalBin(0, 0.5, 2, 1), evalBin(0.5, 1, 1, 1))
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	for name, mutate := range map[string]func(*AttributionEvalSummary){
		"schema":           func(s *AttributionEvalSummary) { s.Schema = "other" },
		"digest":           func(s *AttributionEvalSummary) { s.EvalSetDigest = "" },
		"unattributed":     func(s *AttributionEvalSummary) { s.Unattributed = 4 },
		"target":           func(s *AttributionEvalSummary) { s.Calibration.Target = "" },
		"correct > count":  func(s *AttributionEvalSummary) { s.Calibration.Bins[0].Correct = 3 },
		"overlapping bins": func(s *AttributionEvalSummary) { s.Calibration.Bins[1].Lower = 0.4 },
		"binned count":     func(s *AttributionEvalSummary) { s.CaseCount = 5 },
	} {
		t.Run(name, func(t *testing.T) {
			summary := *calibrationSummary(evalBin(0, 0.5, 2, 1), evalBin(0.5, 1, 1, 1))
			mutate(&summary)
			if err := summary.Validate(); err == nil {
				t.Fatalf("Validate() = nil, want an error for malformed %s", name)
			}
		})
	}
}

func TestAuditFaultDomainsMarksFindingsFileableAgainstThreshold(t *testing.T) {
	var observations []AttributionObservation
	for _, run := range []string{"run-1", "run-2", "run-3"} {
		observations = append(observations, auditObservation(run, "implementation", "version-a", "implement",
			"workflow selected an unsuitable tool", ClassBadToolChoice, 0.55, "stage:implement/tool:one"))
	}
	for _, tc := range []struct {
		name     string
		filing   FilingThreshold
		want     FilingThreshold
		fileable bool
	}{
		{name: "default", want: DeriveFilingThreshold(nil), fileable: true},
		{name: "calibrated above the finding",
			filing:   FilingThreshold{Confidence: 0.6, Source: FilingThresholdAttributionEval, EvalSetVersion: "v1"},
			want:     FilingThreshold{Confidence: 0.6, Source: FilingThresholdAttributionEval, EvalSetVersion: "v1"},
			fileable: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := AuditFaultDomains(observations, FaultAuditConfig{Filing: tc.filing})
			if report.FilingThreshold == nil || *report.FilingThreshold != tc.want {
				t.Fatalf("filing threshold = %+v, want %+v", report.FilingThreshold, tc.want)
			}
			if len(report.WorkflowFindings) != 1 {
				t.Fatalf("report = %+v, want one workflow finding", report)
			}
			if finding := report.WorkflowFindings[0]; finding.Confidence != 0.55 || finding.Fileable != tc.fileable {
				t.Fatalf("finding confidence %v fileable %v, want 0.55 fileable %v", finding.Confidence, finding.Fileable, tc.fileable)
			}
		})
	}
}
