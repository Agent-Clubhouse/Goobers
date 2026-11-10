// Package gatenoise is the gate-noise decision shared by the local rollup
// (rollup.detectGateNoise) and the cloud telemetry store. Both derive a gate's
// findings from its evaluation TOTALS, so cloud sources must report complete
// totals (including sources that produced no finding) and merge them by adding;
// a finding from one source cannot recover another kind's denominator.
package gatenoise

// Totals are one gate's evaluation counts from one or more sources.
type Totals struct {
	Total     int64 // evaluations
	NonPass   int64 // evaluations whose verdict was not pass
	Escalated int64 // evaluations that escalated (repass budget exhausted)
}

// Add combines the counts of two disjoint sources.
func (t Totals) Add(other Totals) Totals {
	return Totals{Total: t.Total + other.Total, NonPass: t.NonPass + other.NonPass, Escalated: t.Escalated + other.Escalated}
}

// Verdict is which gate-noise findings a gate's totals warrant.
type Verdict struct {
	// NeverFails flags a gate with enough evaluations and not one non-pass.
	NeverFails bool
	// Churn flags a gate whose escalation rate meets the threshold.
	Churn          bool
	EscalationRate float64
}

// Evaluate applies the local rules: below minEvaluations nothing is flagged;
// NeverFails when no evaluation failed; Churn when escalated/total reaches
// maxEscalationRate.
func Evaluate(t Totals, minEvaluations int, maxEscalationRate float64) Verdict {
	if t.Total <= 0 || t.Total < int64(minEvaluations) {
		return Verdict{}
	}
	rate := float64(t.Escalated) / float64(t.Total)
	return Verdict{NeverFails: t.NonPass == 0, Churn: rate >= maxEscalationRate, EscalationRate: rate}
}
