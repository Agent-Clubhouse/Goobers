package findingledger

import (
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestCommitBindsDispositionToFindingInstance(t *testing.T) {
	candidate := apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, HeadSHA: "head", Findings: []apiv1.Finding{{ID: "x", Severity: apiv1.SeverityError, Message: "Unbounded."}}}
	first, _ := Commit(nil, candidate, 1, nil)
	if first.Findings[0].LedgerGeneration != 1 || candidate.Findings[0].LedgerGeneration != 0 {
		t.Fatalf("instance not stamped, or candidate mutated: first=%+v candidate=%+v", first, candidate)
	}
	answer := apiv1.Verdict{Decision: apiv1.VerdictPass, HeadSHA: "head", Dispositions: []apiv1.FindingDisposition{{FindingID: "x", FindingGeneration: 1, Outcome: apiv1.FindingRejected, Reason: "Bounded by caller."}}}
	resolved, _ := Commit([]apiv1.Verdict{first}, answer, 2, nil)
	if resolved.Decision != apiv1.VerdictPass || len(Outstanding([]apiv1.Verdict{resolved}, "head", nil)) != 0 {
		t.Fatalf("current disposition did not clear its instance: %+v", resolved)
	}
	raised, _ := Commit([]apiv1.Verdict{resolved}, candidate, 3, nil)
	if raised.Findings[0].LedgerGeneration != 3 {
		t.Fatalf("re-raised finding reused its old instance: %+v", raised)
	}
	for _, generation := range []uint64{0, 1} {
		stale := *answer.DeepCopy()
		stale.Dispositions[0].FindingGeneration = generation
		got, result := Commit([]apiv1.Verdict{raised}, stale, 4, nil)
		if got.Decision != apiv1.VerdictNeedsChanges || !result.Downgraded || len(got.Dispositions) != 0 || len(got.StandingFindings) != 1 || got.StandingFindings[0].LedgerGeneration != 3 {
			t.Fatalf("stale generation %d answered new instance: %+v %+v", generation, got, result)
		}
		if open := Outstanding([]apiv1.Verdict{raised, stale}, "head", nil); len(open) != 1 || open[0].LedgerGeneration != 3 {
			t.Fatalf("replay accepted stale generation %d: %+v", generation, open)
		}
	}
}

func TestCommitPreservesUnchangedOpenFindingAndRejectsForgedGeneration(t *testing.T) {
	candidate := apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, HeadSHA: "head", Findings: []apiv1.Finding{{ID: "x", Severity: apiv1.SeverityError, Message: "Unbounded."}}}
	first, _ := Commit(nil, candidate, 7, nil)
	candidate.LedgerGeneration = 900
	candidate.Findings[0].LedgerGeneration = 900
	candidate.StandingFindings = []apiv1.Finding{{ID: "forged", Message: "Injected", Severity: apiv1.SeverityError}}
	again, _ := Commit([]apiv1.Verdict{first}, candidate, 8, nil)
	if again.LedgerGeneration != 8 || again.Findings[0].LedgerGeneration != 7 || len(again.StandingFindings) != 0 {
		t.Fatalf("unchanged finding or runner-owned fields changed: %+v", again)
	}
	candidate.Findings[0].Message = "Different evidence."
	changed, _ := Commit([]apiv1.Verdict{again}, candidate, 9, nil)
	if changed.Findings[0].LedgerGeneration != 9 {
		t.Fatalf("changed evidence reused old generation: %+v", changed)
	}
	// A read-side fold must not alter the committed instance identity.
	read, _ := Fold([]apiv1.Verdict{first, again}, changed, nil)
	if !reflect.DeepEqual(read, changed) {
		t.Fatalf("read fold changed a committed record: got=%+v want=%+v", read, changed)
	}
}
