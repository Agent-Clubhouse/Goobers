package gate

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestInterruptedMechanicalRecoveryPreservesPriorReview(t *testing.T) {
	g := fixtureSpec().Gates[1]
	g.Branches["defer"], g.Branches["escalate"] = "park", "mechanical-stop"
	prior := apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Rationale: "  Original rationale.\n\nUnabridged evidence.  ", Findings: []apiv1.Finding{{Severity: apiv1.SeverityError, Message: "Original finding."}}}
	ev := &Evaluator{Journal: newTestJournal(t), MaxRepasses: 1, Attempts: map[string]int{g.Name: 2}, RecoveryVerdict: func(string) (*apiv1.Verdict, error) { return &prior, nil }}
	got, recovered, err := ev.RecoverInterrupted(g, "sha256:aaaa")
	if err != nil || !recovered {
		t.Fatalf("recovered=%v err=%v", recovered, err)
	}
	if got.Outcome != "escalate" || got.Target != "mechanical-stop" || got.VerdictArtifact == nil || got.Verdict == nil {
		t.Fatalf("lost mechanical recovery: %+v", got)
	}
	if got.Verdict.ReasonCode != apiv1.VerdictReasonRepassBudget || got.Verdict.Rationale != prior.Rationale || !reflect.DeepEqual(got.Verdict.Findings, prior.Findings) {
		t.Fatalf("lost review evidence: %+v", got.Verdict)
	}
	if prior.Decision != apiv1.VerdictNeedsChanges {
		t.Fatal("mutated prior verdict")
	}
	broken := errors.New("unreadable verdict")
	ev.RecoveryVerdict = func(string) (*apiv1.Verdict, error) { return nil, broken }
	if _, _, err := ev.RecoverInterrupted(g, ""); !errors.Is(err, broken) {
		t.Fatalf("lost evidence silently accepted: %v", err)
	}
}

func TestUninspectedEvidenceUsesMechanicalRouteAndKeepsPriorReview(t *testing.T) {
	g := fixtureSpec().Gates[1]
	g.Branches["defer"], g.Branches["escalate"] = "park", "mechanical-stop"
	prior := apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Rationale: "  Original rationale.\n\nFull detail.  ", Findings: []apiv1.Finding{{Severity: apiv1.SeverityError, Message: "Original finding."}}}
	ev := &Evaluator{Journal: newTestJournal(t), RecoveryVerdict: func(string) (*apiv1.Verdict, error) { return &prior, nil }}
	got, err := ev.EscalateUninspectedRemediation(g, &apiv1.ErrorInfo{Message: "required log was never read"}, 3, "sha256:aaaa")
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != "escalate" || got.Target != "mechanical-stop" || got.VerdictArtifact == nil || got.Verdict == nil {
		t.Fatalf("wrong route: %+v", got)
	}
	if got.Verdict.ReasonCode != apiv1.VerdictReasonEvidenceNotInspected || !strings.Contains(got.Verdict.Rationale, prior.Rationale) || !strings.Contains(got.Verdict.Rationale, "required log was never read") || !reflect.DeepEqual(got.Verdict.Findings, prior.Findings) {
		t.Fatalf("lost evidence: %+v", got.Verdict)
	}
	if prior.Decision != apiv1.VerdictNeedsChanges {
		t.Fatal("mutated prior review")
	}
}

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
