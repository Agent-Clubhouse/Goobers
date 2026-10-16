package harness

import (
	"context"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// #6061: a reviewer that returns "defer" on a gate with no declared defer
// route (the shipped DSL 2.0 merge-review review gate) used to fail the run
// closed with GT-002. The harness maps it to needs-changes, keeping the
// findings, only when the gate did not allow deferral.
func reviewWithVerdict(t *testing.T, returned apiv1.Verdict, deferralAllowed bool) apiv1.Verdict {
	t.Helper()
	rec := &fakeRecorder{}
	adapter := &FakeAdapter{
		Act: func(ctx context.Context, req RunRequest) error {
			return WriteCompletion(req.Workspace, req.CompletionPath, returned)
		},
	}
	exec, err := NewExecutor(adapter, testInjector(t, "", "", noopRegistrar{}), rec, rec, rec, journal.NewPatternScrubber(), "")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	env := testEnvelope(t.TempDir())
	env.ReviewerDeferralAllowed = deferralAllowed
	verdict, err := exec.Review(context.Background(), env)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	return verdict
}

func orderingDefer() apiv1.Verdict {
	return apiv1.Verdict{
		Decision:   apiv1.VerdictDefer,
		ReasonCode: apiv1.VerdictReasonOrdering,
		Rationale:  "wait for the sibling that lands first",
		Findings: []apiv1.Finding{{
			Class:       apiv1.FindingCrossPRBlocked,
			Severity:    apiv1.SeverityInfo,
			Message:     "overlaps sibling PR #73",
			BlockingPRs: []int{73},
		}},
	}
}

func TestReviewMapsUndeclaredDeferralToNeedsChanges(t *testing.T) {
	got := reviewWithVerdict(t, orderingDefer(), false)
	if got.Decision != apiv1.VerdictNeedsChanges {
		t.Fatalf("Decision = %q, want needs-changes: the gate declares no defer route", got.Decision)
	}
	if got.ReasonCode != "" || got.Elected {
		t.Fatalf("ReasonCode=%q Elected=%v, want the deferral-only fields cleared", got.ReasonCode, got.Elected)
	}
	if len(got.Findings) != 1 || got.Findings[0].Class != apiv1.FindingCrossPRBlocked || len(got.Findings[0].BlockingPRs) != 1 {
		t.Fatalf("Findings = %+v, want the reviewer's ordering finding preserved", got.Findings)
	}
	if got.Rationale == "" {
		t.Fatal("Rationale dropped, want the reviewer's rationale preserved")
	}
}

func TestReviewKeepsDeclaredDeferral(t *testing.T) {
	got := reviewWithVerdict(t, orderingDefer(), true)
	if got.Decision != apiv1.VerdictDefer || got.ReasonCode != apiv1.VerdictReasonOrdering {
		t.Fatalf("Decision=%q ReasonCode=%q, want the declared deferral untouched", got.Decision, got.ReasonCode)
	}
}

func TestReviewLeavesOtherDecisionsUntouched(t *testing.T) {
	for _, d := range []apiv1.VerdictDecision{apiv1.VerdictPass, apiv1.VerdictNeedsChanges, apiv1.VerdictFail} {
		got := reviewWithVerdict(t, apiv1.Verdict{Decision: d}, false)
		if got.Decision != d {
			t.Fatalf("Decision %q mapped to %q, want unchanged", d, got.Decision)
		}
	}
}

// #5894: only the runner may mark a verdict synthesized. A reviewer claiming
// it would make its own review read as one that never ran.
func TestReviewClearsReviewerClaimedSynthesizedMarker(t *testing.T) {
	got := reviewWithVerdict(t, apiv1.Verdict{Decision: apiv1.VerdictPass, Synthesized: true}, false)
	if got.Synthesized {
		t.Fatal("Synthesized = true, want the reviewer-supplied marker cleared")
	}
}
