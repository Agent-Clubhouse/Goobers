package gate

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestBudgetMechanicalEscalationPreservesReviewerRationale(t *testing.T) {
	g := fixtureSpec().Gates[1]
	g.Branches["defer"], g.Branches["escalate"] = "park", "mechanical-stop"
	original := "The implementation still needs the missing safety check.\n\nDetailed original evidence."
	reviewer := &fakeGoober{reviewVerdict: apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Rationale: original}}
	ev := &Evaluator{Reviewer: &ReviewerEvaluator{Goober: reviewer}, MaxRepasses: 1, IsReentry: func(target string) bool { return target == "implement" }, Journal: newTestJournal(t)}
	for _, digest := range []string{"sha256:aaaa", "sha256:bbbb"} {
		got, err := ev.Evaluate(context.Background(), g, apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}, digest, false)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(digest, "bbbb") && (got.Outcome != "escalate" || got.Target != "mechanical-stop" || !got.Escalated || got.Verdict.ReasonCode != apiv1.VerdictReasonRepassBudget || got.Verdict.Rationale != original) {
			t.Fatalf("budget exhaustion lost original review: %+v verdict=%+v", got, got.Verdict)
		}
	}
	if reviewer.reviewVerdict.Decision != apiv1.VerdictNeedsChanges || reviewer.reviewVerdict.ReasonCode != "" {
		t.Fatal("mechanical conversion mutated the original review")
	}
}

func TestMechanicalStopsUseSeparateDeclaredRoute(t *testing.T) {
	for _, empty := range []bool{false, true} {
		g := fixtureSpec().Gates[1]
		g.Branches["defer"] = "park"
		g.Branches["escalate"] = "mechanical-stop"
		reviewer := &fakeGoober{}
		ev := &Evaluator{
			Reviewer:       &ReviewerEvaluator{Goober: reviewer},
			LastDiffDigest: map[string]string{g.Name: "sha256:aaaa"},
			IsReentry:      func(string) bool { return false },
			Journal:        newTestJournal(t),
		}
		got, err := ev.Evaluate(context.Background(), g, apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}, "sha256:aaaa", empty)
		if err != nil {
			t.Fatal(err)
		}
		want := apiv1.VerdictReasonUnchangedRepass
		if empty {
			want = apiv1.VerdictReasonEmptyDiff
		}
		if got.Outcome != "escalate" || got.Target != "mechanical-stop" || !got.Escalated || got.Verdict == nil || got.Verdict.ReasonCode != want || got.Verdict.Rationale == "" || got.VerdictArtifact == nil {
			t.Fatalf("mechanical stop was lost or treated as rejection: %+v", got)
		}
		if reviewer.reviewCalls != 0 {
			t.Fatal("mechanical stop invoked the reviewer")
		}
	}
}
