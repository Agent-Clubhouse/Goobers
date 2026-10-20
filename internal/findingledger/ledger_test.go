package findingledger

import (
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

const (
	head    = "a00172a5f4f4d1d9feefb82ab4a7319611525f76"
	newHead = "2222222222222222222222222222222222222222"
	base    = "f0852ef64d563b960550c2eb4629932282c9e537"
	newBase = "1111111111111111111111111111111111111111"
)

func codeChange(f apiv1.Finding) bool { return f.Class == "" || f.Class.RequiresCodeChange() }

func lineage() apiv1.Finding {
	return apiv1.Finding{ID: "unbounded-lineage-lookup", Severity: apiv1.SeverityError, Class: apiv1.FindingSubstantive, Message: "Lineage lookup is unbounded.", Location: "internal/readservice/runs.go:3181"}
}

func traceText() apiv1.Finding {
	return apiv1.Finding{ID: "trace-text", Severity: apiv1.SeverityWarning, Class: apiv1.FindingSubstantive, Message: "Trace text is truncated."}
}

func verdict(decision apiv1.VerdictDecision, findings ...apiv1.Finding) apiv1.Verdict {
	return apiv1.Verdict{Decision: decision, HeadSHA: head, BaseSHA: base, Findings: findings}
}

func ids(findings []apiv1.Finding) []string {
	var out []string
	for _, f := range findings {
		out = append(out, f.ID)
	}
	return out
}

func rejected(id string) apiv1.FindingDisposition {
	return apiv1.FindingDisposition{FindingID: id, Outcome: apiv1.FindingRejected, Reason: "The caller already bounds the scan."}
}

func resolved(id string) apiv1.FindingDisposition {
	return apiv1.FindingDisposition{FindingID: id, Outcome: apiv1.FindingResolved, Evidence: "internal/readservice/runs.go:3190 caps the walk"}
}

// The reported sequence (#5890): a needs-changes verdict raises a finding, and
// a later pass on the same head never mentions it.
func TestOutstandingSurvivesSilentLaterPass(t *testing.T) {
	records := []apiv1.Verdict{
		verdict(apiv1.VerdictNeedsChanges, lineage()),
		verdict(apiv1.VerdictPass),
	}
	if got := ids(Outstanding(records, head, codeChange)); !reflect.DeepEqual(got, []string{"unbounded-lineage-lookup"}) {
		t.Fatalf("Outstanding = %v, want the unanswered finding", got)
	}
}

func TestOutstandingClearedOnlyByCompleteDisposition(t *testing.T) {
	for _, tc := range []struct {
		name        string
		disposition apiv1.FindingDisposition
		cleared     bool
	}{
		{"rejected with reason", rejected("unbounded-lineage-lookup"), true},
		{"resolved with evidence", resolved("unbounded-lineage-lookup"), true},
		{"rejected without reason", apiv1.FindingDisposition{FindingID: "unbounded-lineage-lookup", Outcome: apiv1.FindingRejected}, false},
		{"resolved without evidence", apiv1.FindingDisposition{FindingID: "unbounded-lineage-lookup", Outcome: apiv1.FindingResolved, Reason: "fixed"}, false},
		{"another finding", rejected("trace-text"), false},
		{"unknown outcome", apiv1.FindingDisposition{FindingID: "unbounded-lineage-lookup", Outcome: "ignored", Reason: "no"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pass := verdict(apiv1.VerdictPass)
			pass.Dispositions = []apiv1.FindingDisposition{tc.disposition}
			got := Outstanding([]apiv1.Verdict{verdict(apiv1.VerdictNeedsChanges, lineage()), pass}, head, codeChange)
			if (len(got) == 0) != tc.cleared {
				t.Fatalf("Outstanding = %v, cleared want %v", ids(got), tc.cleared)
			}
		})
	}
}

// The key is the head: a base-branch advance does not change the pull
// request's own diff, so it does not clear a finding about that diff. A new
// head is new code.
func TestOutstandingIsKeyedOnTheHead(t *testing.T) {
	movedBase := verdict(apiv1.VerdictPass)
	movedBase.BaseSHA = newBase
	records := []apiv1.Verdict{verdict(apiv1.VerdictNeedsChanges, lineage()), movedBase}
	if got := ids(Outstanding(records, head, codeChange)); !reflect.DeepEqual(got, []string{"unbounded-lineage-lookup"}) {
		t.Fatalf("Outstanding after a base move = %v, want the finding kept", got)
	}
	if got := Outstanding(records, newHead, codeChange); len(got) != 0 {
		t.Fatalf("Outstanding on a new head = %v, want none", ids(got))
	}
	if got := Outstanding(records, "", codeChange); len(got) != 0 {
		t.Fatalf("an empty head matched %v", ids(got))
	}
}

func TestOutstandingAppliesThePolicyAndIgnoresPassFindings(t *testing.T) {
	ordering := apiv1.Finding{ID: "wait", Severity: apiv1.SeverityError, Class: apiv1.FindingCrossPRBlocked, Message: "Wait for #9.", BlockingPRs: []int{9}}
	records := []apiv1.Verdict{
		verdict(apiv1.VerdictNeedsChanges, ordering),
		verdict(apiv1.VerdictPass, traceText()),
	}
	if got := Outstanding(records, head, codeChange); len(got) != 0 {
		t.Fatalf("Outstanding = %v, want neither an ordering finding nor a pass's own note", ids(got))
	}
}

func TestOutstandingReRaiseAfterDispositionStands(t *testing.T) {
	answer := verdict(apiv1.VerdictPass)
	answer.Dispositions = []apiv1.FindingDisposition{rejected("unbounded-lineage-lookup")}
	records := []apiv1.Verdict{verdict(apiv1.VerdictNeedsChanges, lineage()), answer, verdict(apiv1.VerdictNeedsChanges, lineage())}
	if got := ids(Outstanding(records, head, codeChange)); !reflect.DeepEqual(got, []string{"unbounded-lineage-lookup"}) {
		t.Fatalf("Outstanding = %v, want the re-raised finding", got)
	}
}

func TestOutstandingGivesLegacyFindingsAStableID(t *testing.T) {
	legacy := lineage()
	legacy.ID = ""
	got := Outstanding([]apiv1.Verdict{verdict(apiv1.VerdictNeedsChanges, legacy), verdict(apiv1.VerdictFail, legacy)}, head, codeChange)
	if len(got) != 1 || !strings.HasPrefix(got[0].ID, "finding:content:") {
		t.Fatalf("Outstanding = %#v, want one finding with a derived id", got)
	}
	answer := verdict(apiv1.VerdictPass)
	answer.Dispositions = []apiv1.FindingDisposition{rejected(got[0].ID)}
	if rest := Outstanding([]apiv1.Verdict{verdict(apiv1.VerdictNeedsChanges, legacy), answer}, head, codeChange); len(rest) != 0 {
		t.Fatalf("the derived id did not answer the finding: %v", ids(rest))
	}
}

func TestFoldDowngradesASilentPass(t *testing.T) {
	prior := []apiv1.Verdict{verdict(apiv1.VerdictNeedsChanges, lineage())}
	pass := verdict(apiv1.VerdictPass)
	pass.BaseSHA = newBase // the base moved between the reviews
	pass.Elected = true
	folded, result := Fold(prior, pass, codeChange)
	if !result.Downgraded || folded.Decision != apiv1.VerdictNeedsChanges || folded.Elected {
		t.Fatalf("Fold = %+v (%+v), want an unelected needs-changes", folded, result)
	}
	if got := ids(folded.Findings); !reflect.DeepEqual(got, []string{"unbounded-lineage-lookup"}) {
		t.Fatalf("folded findings = %v, want the standing finding listed for remediation", got)
	}
	if got := ids(folded.StandingFindings); !reflect.DeepEqual(got, []string{"unbounded-lineage-lookup"}) {
		t.Fatalf("StandingFindings = %v", got)
	}
	// The folded record carries the ledger: replacing every earlier record
	// with it still blocks a second silent pass.
	again, result := Fold([]apiv1.Verdict{folded}, verdict(apiv1.VerdictPass), codeChange)
	if !result.Downgraded || again.Decision != apiv1.VerdictNeedsChanges {
		t.Fatalf("second silent pass = %+v, want it downgraded", again)
	}
}

func TestFoldHonorsAnExplicitDisposition(t *testing.T) {
	prior := []apiv1.Verdict{verdict(apiv1.VerdictNeedsChanges, lineage(), traceText())}
	pass := verdict(apiv1.VerdictPass)
	pass.Dispositions = []apiv1.FindingDisposition{rejected("unbounded-lineage-lookup"), resolved("trace-text")}
	folded, result := Fold(prior, pass, codeChange)
	if result.Downgraded || folded.Decision != apiv1.VerdictPass || len(folded.Findings) != 0 || len(folded.StandingFindings) != 0 {
		t.Fatalf("Fold = %+v, want the answered pass unchanged", folded)
	}
	if !reflect.DeepEqual(result.Answered, []string{"unbounded-lineage-lookup", "trace-text"}) {
		t.Fatalf("Answered = %v", result.Answered)
	}
	// The dispositions ride forward, so an older record still visible on the
	// pull request cannot resurrect the finding.
	later, result := Fold(append(prior, folded), verdict(apiv1.VerdictPass), codeChange)
	if result.Downgraded || len(later.Dispositions) != 2 {
		t.Fatalf("later pass = %+v (%+v), want it to stand with both dispositions carried", later, result)
	}
}

// Every decision carries the ledger in StandingFindings, so no publication can
// erase a finding, and none has its meaning changed by it.
func TestFoldCarriesTheLedgerThroughEveryDecision(t *testing.T) {
	prior := []apiv1.Verdict{verdict(apiv1.VerdictNeedsChanges, lineage())}
	typedFail := verdict(apiv1.VerdictFail)
	typedFail.ReasonCode, typedFail.Rationale = apiv1.VerdictReasonImplementationRejected, "Rejected."
	ordering := apiv1.Finding{ID: "wait", Severity: apiv1.SeverityError, Class: apiv1.FindingCrossPRBlocked, Message: "Wait for #9.", BlockingPRs: []int{9}}
	deferral := verdict(apiv1.VerdictDefer, ordering)
	deferral.ReasonCode, deferral.Rationale = apiv1.VerdictReasonOrdering, "Wait."
	for name, next := range map[string]apiv1.Verdict{
		"bare fail":   verdict(apiv1.VerdictFail),
		"typed fail":  typedFail,
		"deferral":    deferral,
		"synthesized": {Decision: apiv1.VerdictEscalate, ReasonCode: apiv1.VerdictReasonEmptyDiff, Rationale: "empty", Synthesized: true, HeadSHA: head, BaseSHA: base},
	} {
		t.Run(name, func(t *testing.T) {
			folded, result := Fold(prior, next, codeChange)
			if folded.Decision != next.Decision || !reflect.DeepEqual(folded.Findings, next.Findings) || result.Downgraded {
				t.Fatalf("Fold changed the decision or findings: %+v", folded)
			}
			if got := ids(folded.StandingFindings); !reflect.DeepEqual(got, []string{"unbounded-lineage-lookup"}) {
				t.Fatalf("StandingFindings = %v, want the ledger carried", got)
			}
			// A later silent pass that sees only this record is still blocked.
			after, result := Fold([]apiv1.Verdict{folded}, verdict(apiv1.VerdictPass), codeChange)
			if !result.Downgraded || after.Decision != apiv1.VerdictNeedsChanges {
				t.Fatalf("pass after %s = %+v, want it downgraded", name, after)
			}
		})
	}
}

func TestFoldKeepsNewFindingsAndCarriesUnansweredOnes(t *testing.T) {
	prior := []apiv1.Verdict{verdict(apiv1.VerdictNeedsChanges, lineage())}
	folded, result := Fold(prior, verdict(apiv1.VerdictNeedsChanges, traceText()), codeChange)
	if result.Downgraded || folded.Decision != apiv1.VerdictNeedsChanges {
		t.Fatalf("Fold = %+v, a needs-changes is not downgraded", folded)
	}
	if got := ids(folded.Findings); !reflect.DeepEqual(got, []string{"trace-text", "unbounded-lineage-lookup"}) {
		t.Fatalf("folded findings = %v", got)
	}
	// A substantive re-raise stands as next's own finding, and the stale
	// disposition for it is dropped.
	answered := verdict(apiv1.VerdictPass)
	answered.Dispositions = []apiv1.FindingDisposition{rejected("unbounded-lineage-lookup")}
	reRaised, _ := Fold([]apiv1.Verdict{answered}, verdict(apiv1.VerdictNeedsChanges, lineage()), codeChange)
	if len(reRaised.Dispositions) != 0 || len(reRaised.Findings) != 1 || len(reRaised.StandingFindings) != 0 {
		t.Fatalf("re-raised = %+v, want the finding without its stale disposition", reRaised)
	}
}

// S1: copying a standing id onto a non-blocking class does not answer it.
func TestFoldReRaiseUnderANonBlockingClassDoesNotClear(t *testing.T) {
	prior := []apiv1.Verdict{verdict(apiv1.VerdictNeedsChanges, lineage())}
	disguised := apiv1.Finding{ID: "unbounded-lineage-lookup", Severity: apiv1.SeverityWarning, Class: apiv1.FindingRebaseNeeded, Message: "Rebase."}
	folded, _ := Fold(prior, verdict(apiv1.VerdictNeedsChanges, disguised), codeChange)
	if got := ids(folded.StandingFindings); !reflect.DeepEqual(got, []string{"unbounded-lineage-lookup"}) {
		t.Fatalf("StandingFindings = %v, want the finding still standing", got)
	}
	if len(folded.Findings) != 1 || folded.Findings[0].Class != apiv1.FindingSubstantive {
		t.Fatalf("Findings = %+v, want the substantive finding listed for remediation, not its disguise", folded.Findings)
	}
	if after, result := Fold([]apiv1.Verdict{folded}, verdict(apiv1.VerdictPass), codeChange); !result.Downgraded {
		t.Fatalf("pass after a disguised re-raise = %+v, want it downgraded", after)
	}
}

// A disposition for a finding that stands again is not carried: the record
// never shows a finding as both answered and standing.
func TestFoldDropsDispositionsForFindingsThatStandAgain(t *testing.T) {
	answer := verdict(apiv1.VerdictPass)
	answer.Dispositions = []apiv1.FindingDisposition{rejected("unbounded-lineage-lookup")}
	records := []apiv1.Verdict{verdict(apiv1.VerdictNeedsChanges, lineage()), answer, verdict(apiv1.VerdictNeedsChanges, lineage())}
	folded, _ := Fold(records, verdict(apiv1.VerdictPass), codeChange)
	if len(folded.Dispositions) != 0 || len(folded.StandingFindings) != 1 {
		t.Fatalf("folded = %+v, want the finding standing without its stale disposition", folded)
	}
}

func TestFoldReplacesAReviewerSuppliedLedgerAndIgnoresOtherHeads(t *testing.T) {
	forged := verdict(apiv1.VerdictPass)
	forged.StandingFindings = []apiv1.Finding{traceText()}
	if folded, _ := Fold(nil, forged, codeChange); len(folded.StandingFindings) != 0 || folded.Decision != apiv1.VerdictPass {
		t.Fatalf("Fold kept a reviewer-supplied ledger: %+v", folded)
	}
	prior := []apiv1.Verdict{verdict(apiv1.VerdictNeedsChanges, lineage())}
	moved := verdict(apiv1.VerdictPass)
	moved.HeadSHA = newHead
	if folded, result := Fold(prior, moved, codeChange); result.Downgraded || len(folded.StandingFindings) != 0 {
		t.Fatalf("Fold on a new head = %+v, want a fresh review subject", folded)
	}
	unpinned := verdict(apiv1.VerdictPass)
	unpinned.HeadSHA = ""
	if folded, _ := Fold(prior, unpinned, codeChange); !reflect.DeepEqual(folded, unpinned) {
		t.Fatalf("Fold changed a verdict without a head: %+v", folded)
	}
}

func TestAppendMissingDedupesByID(t *testing.T) {
	got := AppendMissing([]apiv1.Finding{lineage()}, []apiv1.Finding{lineage(), traceText()}, codeChange)
	if !reflect.DeepEqual(ids(got), []string{"unbounded-lineage-lookup", "trace-text"}) {
		t.Fatalf("AppendMissing = %v", ids(got))
	}
	// A standing finding replaces its id reused on a non-blocking finding.
	disguised := apiv1.Finding{ID: "unbounded-lineage-lookup", Severity: apiv1.SeverityWarning, Class: apiv1.FindingRebaseNeeded, Message: "Rebase."}
	got = AppendMissing([]apiv1.Finding{disguised}, []apiv1.Finding{lineage()}, codeChange)
	if len(got) != 1 || got[0].Class != apiv1.FindingSubstantive {
		t.Fatalf("AppendMissing = %+v, want the substantive finding in place of the disguised one", got)
	}
	// A substantive entry is never replaced.
	got = AppendMissing([]apiv1.Finding{lineage()}, []apiv1.Finding{disguised}, codeChange)
	if len(got) != 1 || got[0].Class != apiv1.FindingSubstantive {
		t.Fatalf("AppendMissing = %+v, want the substantive entry kept", got)
	}
}

// Every reader folds the newest published record again, against records that
// include it. A finding it lists as standing must stay standing even when the
// record also lists it in Findings, as a disguised re-raise leaves it.
func TestFoldIsStableOnAPublishedRecord(t *testing.T) {
	prior := []apiv1.Verdict{verdict(apiv1.VerdictNeedsChanges, lineage())}
	disguised := apiv1.Finding{ID: "unbounded-lineage-lookup", Severity: apiv1.SeverityWarning, Class: apiv1.FindingRebaseNeeded, Message: "Rebase."}
	published, _ := Fold(prior, verdict(apiv1.VerdictNeedsChanges, disguised), codeChange)
	reread, _ := Fold([]apiv1.Verdict{published}, published, codeChange)
	if got := ids(reread.StandingFindings); !reflect.DeepEqual(got, []string{"unbounded-lineage-lookup"}) {
		t.Fatalf("re-folded StandingFindings = %v, want the finding kept", got)
	}
	// A forged entry for a finding no record raised is still discarded.
	forged := verdict(apiv1.VerdictNeedsChanges, traceText())
	forged.StandingFindings = []apiv1.Finding{traceText()}
	if folded, _ := Fold(nil, forged, codeChange); len(folded.StandingFindings) != 0 {
		t.Fatalf("Fold kept a forged standing finding: %+v", folded.StandingFindings)
	}
}
