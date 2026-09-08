package readservice

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestTerminalReviewPreservesDispositionAndEvidence(t *testing.T) {
	for _, phase := range []journal.RunPhase{journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseAborted, journal.PhaseEscalated} {
		for _, disposition := range []struct {
			decision apiv1.VerdictDecision
			reason   apiv1.VerdictReasonCode
		}{
			{apiv1.VerdictPass, ""},
			{apiv1.VerdictNeedsChanges, ""},
			{apiv1.VerdictFail, apiv1.VerdictReasonImplementationRejected},
			{apiv1.VerdictFail, apiv1.VerdictReasonPolicyRejected},
			{apiv1.VerdictFail, ""},
			{apiv1.VerdictDefer, apiv1.VerdictReasonOrdering},
			{apiv1.VerdictDefer, apiv1.VerdictReasonNoLander},
			{apiv1.VerdictEscalate, apiv1.VerdictReasonEmptyDiff},
			{apiv1.VerdictEscalate, apiv1.VerdictReasonUnchangedRepass},
			{apiv1.VerdictEscalate, apiv1.VerdictReasonRepassBudget},
			{apiv1.VerdictEscalate, apiv1.VerdictReasonFindingOscillation},
		} {
			t.Run(string(phase)+"/"+string(disposition.decision)+"/"+string(disposition.reason), func(t *testing.T) {
				service, layout, machine := fixtureService(t)
				run, clock := createFixtureRun(t, layout, machine, "terminal-review", "implementation", "goobers", fixedTime, journal.Trigger{Kind: journal.TriggerItem, Ref: "4495"}, true)
				verdict := apiv1.Verdict{
					Decision: disposition.decision, ReasonCode: disposition.reason,
					Rationale: "  Original review.\n\nDetailed reasoning must survive terminal disposition.\n",
					Findings:  []apiv1.Finding{{Severity: apiv1.SeverityInfo, Class: apiv1.FindingCrossPRBlocked, Message: "Review evidence.", BlockingPRs: []int{10}}},
				}
				data, err := json.Marshal(verdict)
				if err != nil {
					t.Fatal(err)
				}
				ref, err := run.RecordArtifact("review-verdict.json", data)
				if err != nil {
					t.Fatal(err)
				}
				if err := run.Append(journal.Event{Type: journal.EventGateEvaluated, Gate: "review", Verdict: string(verdict.Decision), Ref: &ref}); err != nil {
					t.Fatal(err)
				}
				finishFixtureRun(t, run, clock, phase)
				service.now = func() time.Time { return fixedTime.Add(time.Hour) }
				rows, err := service.ListStatusRuns(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) != 1 || rows[0].Operator.Review == nil {
					t.Fatalf("missing terminal review: %+v", rows)
				}
				got := rows[0].Operator.Review
				if got.Verdict != string(verdict.Decision) || got.ReasonCode != verdict.ReasonCode || got.Rationale != verdict.Rationale || !reflect.DeepEqual(got.Findings, verdict.Findings) {
					t.Fatalf("terminal review lost evidence: got=%+v want=%+v", got, verdict)
				}
				if got.LegacyFailAmbiguous != (verdict.Decision == apiv1.VerdictFail && verdict.ReasonCode == "") {
					t.Fatalf("wrong legacy ambiguity: %+v", got)
				}
			})
		}
	}
}
