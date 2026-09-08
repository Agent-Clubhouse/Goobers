package prqueue

import "testing"

func TestReportBoundsObservedItemsWithoutPretendingCompleteCoverage(t *testing.T) {
	r := Report{Version: 1}
	for n := 1; n <= MaxReportItems+23; n++ {
		r.Add(n, Escalated)
	}
	if len(r.Items) != MaxReportItems || r.MatchingItems != MaxReportItems+23 || r.OmittedItems != 23 || r.CompleteSnapshot {
		t.Fatalf("incorrect bounded observation: items=%d matching=%d omitted=%d complete=%t", len(r.Items), r.MatchingItems, r.OmittedItems, r.CompleteSnapshot)
	}
	if r.Items[0].Eligible || r.Items[0].NextStep != NextStep(Escalated) || r.Items[0].Claim.State != "unknown" {
		t.Fatalf("classification lost or unknown claim made available: %+v", r.Items[0])
	}
}

func TestPolicyEligibilityDoesNotAssertClaimAvailability(t *testing.T) {
	r := Report{Version: 1, CompleteSnapshot: true}
	r.Add(42, "")
	if !r.Items[0].Eligible || r.Items[0].Claim.State != "unknown" || r.Items[0].Reason != "" {
		t.Fatalf("eligibility conflated with claim availability: %+v", r.Items[0])
	}
}
