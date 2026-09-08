package gate

import (
	"context"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	wf "github.com/goobers/goobers/internal/workflow"
)

func TestReviewerDeferralWithoutDeclaredRouteFailsClosed(t *testing.T) {
	g := fixtureSpec().Gates[1]
	ev := &Evaluator{Reviewer: &ReviewerEvaluator{Goober: &fakeGoober{reviewVerdict: apiv1.Verdict{
		Decision: apiv1.VerdictDefer, ReasonCode: apiv1.VerdictReasonOrdering, Rationale: "Wait for the sibling.",
	}}}}
	_, err := ev.Evaluate(context.Background(), g, apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}, "sha256:aaaa", false)
	if err == nil || !strings.Contains(err.Error(), "no defined branch") {
		t.Fatalf("undeclared deferral did not fail closed: %v", err)
	}
}

func TestReviewerDeferralRoutesWithoutRejectionOrRepass(t *testing.T) {
	for _, reason := range []apiv1.VerdictReasonCode{apiv1.VerdictReasonOrdering, apiv1.VerdictReasonNoLander} {
		t.Run(string(reason), func(t *testing.T) {
			verdict := apiv1.Verdict{
				Decision: apiv1.VerdictDefer, ReasonCode: reason,
				Rationale: "Wait for the sibling.\n\nKeep the complete review evidence.",
				Findings:  []apiv1.Finding{{Severity: apiv1.SeverityInfo, Message: "Sibling must land first", Class: apiv1.FindingCrossPRBlocked, BlockingPRs: []int{10}}},
			}
			g := apiv1.Gate{
				Name: "review", Evaluator: apiv1.EvaluatorAgentic,
				Agentic:  &apiv1.AgenticGate{Goober: "reviewer"},
				Branches: map[string]string{"pass": wf.TerminalComplete, "fail": wf.TargetAbort, "needs-changes": "implement", "defer": "park"},
			}
			ev := &Evaluator{
				Reviewer: &ReviewerEvaluator{Goober: &fakeGoober{reviewVerdict: verdict}}, Journal: newTestJournal(t),
				// Match runner history: implementation has completed, parking has not.
				IsReentry: func(target string) bool { return target == "implement" },
			}
			got, err := ev.Evaluate(context.Background(), g, apiv1.InvocationEnvelope{}, "implement", apiv1.ResultEnvelope{}, "sha256:aaaa", false)
			if err != nil {
				t.Fatal(err)
			}
			if got.Outcome != "defer" || got.Target != "park" || got.Escalated || got.Attempt != 0 {
				t.Fatalf("deferral was reclassified or charged a repass: %+v", got)
			}
			if got.Verdict == nil || len(got.Verdict.Findings) != 1 || got.VerdictArtifact == nil {
				t.Fatalf("missing verdict evidence: %+v", got)
			}
			// The learning pipeline enriches identities; it must not alter the
			// reviewer-authored content while doing so.
			finding := got.Verdict.Findings[0]
			if finding.ID == "" || finding.LearningSignature == "" || finding.EvidenceDigest != "sha256:aaaa" {
				t.Fatalf("missing learning identity: %+v", finding)
			}
			finding.ID, finding.LearningSignature, finding.LearningClassification, finding.EvidenceDigest = "", "", "", ""
			preserved := *got.Verdict
			preserved.Findings = []apiv1.Finding{finding}
			if !reflect.DeepEqual(preserved, verdict) {
				t.Fatalf("deferral lost evidence or durable verdict artifact: got=%+v want=%+v artifact=%+v", got.Verdict, verdict, got.VerdictArtifact)
			}
		})
	}
}
