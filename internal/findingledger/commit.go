package findingledger

import (
	"reflect"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// Commit folds a reviewer candidate against the records observed inside the
// authoritative transaction. generation belongs to that transaction, never to
// the reviewer. Repeating an unchanged, still-open finding preserves its
// instance; raising it after resolution, or with changed evidence, creates a
// new instance that an old disposition cannot answer.
func Commit(prior []apiv1.Verdict, candidate apiv1.Verdict, generation uint64, substantive Substantive) (apiv1.Verdict, FoldResult) {
	next := *candidate.DeepCopy()
	next.LedgerGeneration = generation
	next.StandingFindings = nil
	open := map[string]apiv1.Finding{}
	for _, finding := range Outstanding(prior, next.HeadSHA, substantive) {
		open[finding.ID] = finding
	}
	for i := range next.Findings {
		finding := &next.Findings[i]
		finding.LedgerGeneration = 0
		if next.Decision == apiv1.VerdictPass || !allows(substantive, *finding) {
			continue
		}
		finding.ID = FindingID(*finding)
		finding.LedgerGeneration = generation
		if previous, ok := open[finding.ID]; ok && sameFinding(previous, *finding) {
			finding.LedgerGeneration = previous.LedgerGeneration
		}
	}
	return Fold(prior, next, substantive)
}

func sameFinding(a, b apiv1.Finding) bool {
	a.LedgerGeneration, b.LedgerGeneration = 0, 0
	return reflect.DeepEqual(a, b)
}
