package findingledger

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"testing"
)

func TestCommitResolvedNewInstanceDoesNotResurrectStaleProjection(t *testing.T) {
	candidate := apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, HeadSHA: "head", Findings: []apiv1.Finding{{ID: "x", Severity: apiv1.SeverityError, Message: "Defect."}}}
	first, _ := Commit(nil, candidate, 1, nil)
	answer := apiv1.Verdict{Decision: apiv1.VerdictPass, HeadSHA: "head", Dispositions: []apiv1.FindingDisposition{{FindingID: "x", FindingGeneration: 1, Outcome: apiv1.FindingRejected, Reason: "Evidence."}}}
	resolved, _ := Commit([]apiv1.Verdict{first}, answer, 2, nil)
	raised, _ := Commit([]apiv1.Verdict{first, resolved}, candidate, 3, nil)
	answer.Dispositions[0].FindingGeneration = 3
	resolvedAgain, _ := Commit([]apiv1.Verdict{first, raised}, answer, 4, nil)
	if resolvedAgain.Decision != apiv1.VerdictPass {
		t.Fatalf("expected resolved latest verdict, got %+v", resolvedAgain)
	}
	for _, records := range [][]apiv1.Verdict{{first, resolvedAgain}, {resolvedAgain, first}, {raised, resolvedAgain, first}} {
		if open := Outstanding(records, "head", nil); len(open) != 0 {
			t.Fatalf("old provider projection resurrected resolved instance: %+v", open)
		}
	}
}

func TestCommitPassFindingCannotDowngradeStandingGeneration(t *testing.T) {
	candidate := apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, HeadSHA: "head", Findings: []apiv1.Finding{{ID: "x", Severity: apiv1.SeverityError, Message: "Defect."}}}
	first, _ := Commit(nil, candidate, 1, nil)
	candidate.Decision = apiv1.VerdictPass
	candidate.Findings[0].Message = "A pass note with the same ID."
	downgraded, _ := Commit([]apiv1.Verdict{first}, candidate, 2, nil)
	open := Outstanding([]apiv1.Verdict{downgraded}, "head", nil)
	if len(open) != 1 || open[0].LedgerGeneration != 1 || open[0].Message != "Defect." || downgraded.StandingFindings[0].Message != "Defect." {
		t.Fatalf("pass note overwrote committed standing generation: %+v (verdict %+v)", open, downgraded)
	}
}

func TestCommitMigratesLegacyFindingsBeforeSnapshotAuthority(t *testing.T) {
	legacy := apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, HeadSHA: "head", Findings: []apiv1.Finding{{ID: "legacy", Severity: apiv1.SeverityError, Message: "Legacy defect."}}}
	silent := apiv1.Verdict{Decision: apiv1.VerdictPass, HeadSHA: "head"}
	first, _ := Commit([]apiv1.Verdict{legacy, silent}, silent, 1, nil)
	if first.Decision != apiv1.VerdictNeedsChanges || len(first.StandingFindings) != 1 || first.StandingFindings[0].LedgerGeneration != 0 {
		t.Fatalf("migration lost legacy finding: %+v", first)
	}
	answer := silent
	answer.Dispositions = []apiv1.FindingDisposition{{FindingID: "legacy", Outcome: apiv1.FindingRejected, Reason: "Caller bounds it."}}
	resolved, _ := Commit([]apiv1.Verdict{legacy, first}, answer, 2, nil)
	if resolved.Decision != apiv1.VerdictPass {
		t.Fatalf("legacy answer did not clear legacy instance: %+v", resolved)
	}
	for _, records := range [][]apiv1.Verdict{{legacy, resolved}, {resolved, legacy}, {first, resolved, legacy}} {
		if open := Outstanding(records, "head", nil); len(open) != 0 {
			t.Fatalf("stale legacy projection resurrected migrated finding: %+v", open)
		}
	}
}

func TestCommitOlderSnapshotCannotReplaceNewInstance(t *testing.T) {
	candidate := apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, HeadSHA: "head", Findings: []apiv1.Finding{{ID: "x", Severity: apiv1.SeverityError, Message: "Old evidence."}}}
	first, _ := Commit(nil, candidate, 1, nil)
	candidate.Findings[0].Message = "New evidence."
	latest, _ := Commit([]apiv1.Verdict{first}, candidate, 2, nil)
	open := Outstanding([]apiv1.Verdict{latest, first}, "head", nil)
	if len(open) != 1 || open[0].LedgerGeneration != 2 || open[0].Message != "New evidence." {
		t.Fatalf("late old snapshot replaced new instance: %+v", open)
	}
}
