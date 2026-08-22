package readmodel

import (
	"math"
	"testing"
)

func TestEstimateCausalCreditDifferenceInDifferences(t *testing.T) {
	var observations []CausalObservation
	for i := 0; i < 10; i++ {
		observations = append(observations,
			CausalObservation{Node: "review", Outcome: 0, Treated: true, Covariates: map[string]string{"repo": "a"}},
			CausalObservation{Node: "review", Outcome: 1, Treated: true, Post: true, Covariates: map[string]string{"repo": "a"}},
			CausalObservation{Node: "review", Outcome: 0, Treated: false, Covariates: map[string]string{"repo": "a"}},
			CausalObservation{Node: "review", Outcome: 0, Treated: false, Post: true, Covariates: map[string]string{"repo": "a"}},
		)
	}
	got, err := EstimateCausalCredit(observations, CausalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Identification != CausalDifferenceInDifferences {
		t.Fatalf("estimate = %+v, want identified DiD", got)
	}
	if got[0].Effect != 1 || got[0].Lower != 1 || got[0].Upper != 1 {
		t.Fatalf("interval = %.3f [%.3f, %.3f], want [1, 1]", got[0].Effect, got[0].Lower, got[0].Upper)
	}
	if !got[0].PromotionEligible || got[0].PromotionSource != string(CausalDifferenceInDifferences) {
		t.Fatalf("promotion contract = %+v", got[0])
	}
}

func TestEstimateCausalCreditRetainsIneligibleFallbackWhenNodeIsAlwaysRouted(t *testing.T) {
	var observations []CausalObservation
	for i := 0; i < 10; i++ {
		observations = append(observations,
			CausalObservation{Node: "review", Outcome: 0, Treated: true, Covariates: map[string]string{"repo": "a"}},
			CausalObservation{Node: "review", Outcome: 1, Treated: true, Post: true, Covariates: map[string]string{"repo": "a"}},
		)
	}
	got, err := EstimateCausalCredit(observations, CausalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Identification != CausalUnidentifiable {
		t.Fatalf("estimate = %+v, want unidentifiable fallback", got)
	}
	if got[0].TreatedBefore != 10 || got[0].TreatedAfter != 10 ||
		got[0].Effect != 1 || got[0].IntervalAvailable ||
		got[0].PromotionEligible || got[0].PromotionSource != "correlational-fallback" {
		t.Fatalf("fallback = %+v, want preserved value without promotion eligibility", got[0])
	}
}

func TestEstimateCausalCreditRejectsIdentityContrastWithoutOverlap(t *testing.T) {
	var observations []CausalObservation
	for i := 0; i < 10; i++ {
		observations = append(observations,
			CausalObservation{Node: "review", Outcome: 0, Treated: false, Covariates: map[string]string{"parent": "old"}},
			CausalObservation{Node: "review", Outcome: 1, Treated: true, Post: true, Covariates: map[string]string{"parent": "new"}},
		)
	}
	got, err := EstimateCausalCredit(observations, CausalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Identification != CausalUnidentifiable {
		t.Fatalf("estimate = %+v, want overlap failure", got[0])
	}
}

func TestEstimateCausalCreditRequiresCovariatesForObservational(t *testing.T) {
	var observations []CausalObservation
	for i := 0; i < 10; i++ {
		observations = append(observations,
			CausalObservation{Node: "review", Outcome: 0, Treated: true},
			CausalObservation{Node: "review", Outcome: 1, Treated: true, Post: true},
			CausalObservation{Node: "review", Outcome: 0, Treated: false},
			CausalObservation{Node: "review", Outcome: 0, Treated: false, Post: true},
		)
	}
	got, err := EstimateCausalCredit(observations, CausalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Identification != CausalUnidentifiable {
		t.Fatalf("estimate = %+v, want missing-covariate unidentifiable", got[0])
	}
}

func TestEstimateCausalCreditRandomizedAndMinimumCohort(t *testing.T) {
	var observations []CausalObservation
	for i := 0; i < 10; i++ {
		observations = append(observations,
			CausalObservation{Node: "implement", Outcome: 0, Randomized: true, Arm: "control", Post: true},
			CausalObservation{Node: "implement", Outcome: 1, Randomized: true, Arm: "treatment", Post: true},
		)
	}
	got, err := EstimateCausalCredit(observations, CausalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Identification != CausalRandomized || got[0].Effect != 1 {
		t.Fatalf("randomized estimate = %+v", got[0])
	}

	got, err = EstimateCausalCredit(observations[:2], CausalOptions{MinCohortSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Identification != CausalUnidentifiable || got[0].Caveat == "" {
		t.Fatalf("thin estimate = %+v, want explicit cannot-identify caveat", got[0])
	}
}

func TestEstimateCausalCreditRejectsNonFiniteInput(t *testing.T) {
	_, err := EstimateCausalCredit([]CausalObservation{{Node: "x", Outcome: math.Inf(1)}}, CausalOptions{})
	if err == nil {
		t.Fatal("expected non-finite outcome error")
	}
}

func TestEstimateCausalCreditChecksCovariateOverlap(t *testing.T) {
	var observations []CausalObservation
	for i := 0; i < 10; i++ {
		observations = append(observations,
			CausalObservation{Node: "x", Treated: true, Covariates: map[string]string{"repo": "a"}},
			CausalObservation{Node: "x", Treated: true, Post: true, Covariates: map[string]string{"repo": "a"}},
			CausalObservation{Node: "x", Covariates: map[string]string{"repo": "b"}},
			CausalObservation{Node: "x", Post: true, Covariates: map[string]string{"repo": "b"}},
		)
	}
	got, err := EstimateCausalCredit(observations, CausalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Identification != CausalUnidentifiable {
		t.Fatalf("estimate = %+v, want overlap failure", got[0])
	}
}

func TestEstimateCausalCreditRejectsMissingCovariateOverlap(t *testing.T) {
	var observations []CausalObservation
	for i := 0; i < 10; i++ {
		observations = append(observations,
			CausalObservation{Node: "x", Treated: true, Covariates: map[string]string{"repo": "a"}},
			CausalObservation{Node: "x", Treated: true, Post: true, Covariates: map[string]string{"repo": "a"}},
			CausalObservation{Node: "x", Covariates: map[string]string{}},
			CausalObservation{Node: "x", Post: true, Covariates: map[string]string{}},
		)
	}
	got, err := EstimateCausalCredit(observations, CausalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Identification != CausalUnidentifiable {
		t.Fatalf("estimate = %+v, want missing-value overlap failure", got[0])
	}
}

func TestCausalCreditDiscoversDifferenceInDifferencesFromChangepoints(t *testing.T) {
	// Integration test: verify that the estimator correctly identifies
	// difference-in-differences effects from observations of a node that
	// changes identity (intervention) for some runs but not others (control).
	var observations []CausalObservation

	// Pre-intervention period: all runs have identity v1 (treated=false means no change yet)
	for i := 0; i < 10; i++ {
		observations = append(observations,
			CausalObservation{
				Node: "stage:implement", Outcome: 1, Treated: true, Post: false,
				Randomized: false, Covariates: map[string]string{"workflow": "test"},
			})
	}

	// Post-intervention: runs that experienced identity change to v2 (treated=true)
	for i := 0; i < 10; i++ {
		observations = append(observations,
			CausalObservation{
				Node: "stage:implement", Outcome: 0, Treated: true, Post: true,
				Randomized: false, Covariates: map[string]string{"workflow": "test"},
			})
	}

	// Pre-intervention control: runs that never got updated (treated=false, pre)
	for i := 0; i < 10; i++ {
		observations = append(observations,
			CausalObservation{
				Node: "stage:implement", Outcome: 1, Treated: false, Post: false,
				Randomized: false, Covariates: map[string]string{"workflow": "test"},
			})
	}

	// Post-intervention control: runs that stayed with v1 (treated=false, no change)
	for i := 0; i < 10; i++ {
		observations = append(observations,
			CausalObservation{
				Node: "stage:implement", Outcome: 1, Treated: false, Post: true,
				Randomized: false, Covariates: map[string]string{"workflow": "test"},
			})
	}

	got, err := EstimateCausalCredit(observations, CausalOptions{MinCohortSize: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("estimates = %d nodes, want 1, got: %+v", len(got), got)
	}
	// DiD effect = (treated_after - treated_before) - (control_after - control_before)
	//            = (0 - 1) - (1 - 1) = -1 - 0 = -1.0
	if got[0].Identification != CausalDifferenceInDifferences {
		t.Fatalf("identification = %s, want difference-in-differences, caveat=%s, cohorts=%d/%d/%d/%d",
			got[0].Identification, got[0].Caveat, got[0].TreatedBefore, got[0].TreatedAfter,
			got[0].ControlBefore, got[0].ControlAfter)
	}
	if got[0].Effect != -1 {
		t.Fatalf("effect = %.3f, want -1.0", got[0].Effect)
	}
}
