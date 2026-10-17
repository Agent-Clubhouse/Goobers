// Package findingledger decides which merge-review findings still stand on a
// pull request head when several instance-authored verdicts exist for it
// (#5890).
//
// The rule is deterministic. Verdict records for one pull request are read in
// publication order, and only records pinned to the head being judged take
// part. The base is not part of the key: a base-branch advance does not change
// the pull request's own diff, so it does not clear a finding about that diff.
// A new head is new code, which the next review judges in full.
//
// For each record, in order:
//
//   - Its complete dispositions (resolved with evidence, rejected with a
//     reason) remove the findings they name.
//   - Its standing findings, the ledger it carried when published, stand.
//   - If it is not a pass, its own substantive findings stand.
//
// A pass raises nothing, and a pass that says nothing about a finding does not
// remove it. Only a disposition does. What stands at the end is outstanding:
// it blocks merge authorization and keeps a healer from treating the pull
// request as mechanically healed.
//
// Fold applies the same rule at publication time and writes the result into
// the published verdict's StandingFindings, whatever its decision. The newest
// record therefore carries the whole ledger, so a later record of any kind can
// replace an earlier one in a single-slot store without erasing a finding.
package findingledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// Substantive decides whether a finding is one that blocks: the caller's
// remediation policy (class, severity floor, which pull request it is about).
type Substantive func(apiv1.Finding) bool

// FindingID returns the identity a disposition must name to answer f. The
// runner assigns every reviewer finding an id; a legacy finding without one
// gets a stable id derived from its content.
func FindingID(f apiv1.Finding) string {
	if id := strings.TrimSpace(f.ID); id != "" {
		return id
	}
	data, _ := json.Marshal(struct {
		Severity apiv1.Severity     `json:"severity"`
		Class    apiv1.FindingClass `json:"class"`
		Message  string             `json:"message"`
		Location string             `json:"location"`
	}{f.Severity, f.Class, strings.TrimSpace(f.Message), strings.TrimSpace(f.Location)})
	sum := sha256.Sum256(data)
	return "finding:content:" + hex.EncodeToString(sum[:])
}

// Outstanding returns the substantive findings still standing on head after
// records, in publication order (oldest first). Records pinned to another head
// are ignored, and an empty head matches nothing. The result is ordered by
// first raise and has one entry per finding id, each carrying its id.
func Outstanding(records []apiv1.Verdict, head string, substantive Substantive) []apiv1.Finding {
	return replay(records, head, substantive).findings()
}

// FoldResult reports what Fold changed.
type FoldResult struct {
	// Standing are the ids of the earlier findings next leaves standing,
	// now recorded in its StandingFindings.
	Standing []string
	// Answered are the ids of earlier outstanding findings next
	// dispositioned.
	Answered []string
	// Downgraded is true when next was a pass and a standing finding made
	// it needs-changes.
	Downgraded bool
}

// Fold makes next, the verdict about to be published for its head, carry the
// ledger of the earlier records on that head:
//
//   - next.StandingFindings becomes every earlier outstanding finding that
//     next neither dispositions nor raises again under a substantive class.
//     A finding next already lists as standing stays standing even when
//     next also raises it, so folding a published record again (as every
//     reader does) never drops one. Anything else in a reviewer-supplied
//     value is discarded: it cannot add or clear a finding.
//   - next.Dispositions keeps every earlier disposition, except one for a
//     finding that stands again.
//   - On a pass or a needs-changes, the standing findings are also listed in
//     Findings, so remediation sees them, and a pass becomes needs-changes:
//     its review did not answer a finding that still stands. Every other
//     decision keeps its findings and meaning; StandingFindings alone carries
//     the ledger through it.
//
// A next without a head is returned unchanged.
func Fold(prior []apiv1.Verdict, next apiv1.Verdict, substantive Substantive) (apiv1.Verdict, FoldResult) {
	head := next.HeadSHA
	if head == "" {
		return next, FoldResult{}
	}
	answered := answeredIDs(next.Dispositions)
	ownRaised := map[string]bool{}
	if next.Decision != apiv1.VerdictPass {
		for _, finding := range next.Findings {
			if allows(substantive, finding) {
				ownRaised[FindingID(finding)] = true
			}
		}
	}

	alreadyStanding := map[string]bool{}
	for _, finding := range next.StandingFindings {
		alreadyStanding[FindingID(finding)] = true
	}

	var result FoldResult
	var standing []apiv1.Finding
	for _, finding := range Outstanding(prior, head, substantive) {
		switch id := finding.ID; {
		case answered[id]:
			result.Answered = append(result.Answered, id)
		case ownRaised[id] && !alreadyStanding[id]:
			// next raises it again itself; its own copy stands.
		default:
			standing = append(standing, finding)
			result.Standing = append(result.Standing, id)
		}
	}
	next.StandingFindings = standing

	stillOpen := map[string]bool{}
	for id := range ownRaised {
		stillOpen[id] = true
	}
	for _, id := range result.Standing {
		stillOpen[id] = true
	}
	dispositions := mergeDispositions(dispositionsOn(prior, head), next.Dispositions)
	next.Dispositions = slices.DeleteFunc(dispositions, func(d apiv1.FindingDisposition) bool {
		return stillOpen[d.FindingID]
	})
	if len(next.Dispositions) == 0 {
		next.Dispositions = nil
	}

	if len(standing) > 0 && !next.Synthesized && (next.Decision == apiv1.VerdictPass || next.Decision == apiv1.VerdictNeedsChanges) {
		next.Findings = appendMissing(next.Findings, standing, substantive)
		if next.Decision == apiv1.VerdictPass {
			next.Decision = apiv1.VerdictNeedsChanges
			next.Elected = false
			result.Downgraded = true
		}
	}
	return next, result
}

// AppendMissing appends to findings each of extra whose id findings does not
// already hold. A substantive finding in extra replaces a same-id entry that
// is not substantive: a standing finding's id reused on a non-blocking
// finding (a rebase-needed one, say) must not hide the finding itself from
// the readers that route on findings.
func AppendMissing(findings, extra []apiv1.Finding, substantive Substantive) []apiv1.Finding {
	return appendMissing(findings, extra, substantive)
}

func appendMissing(findings, extra []apiv1.Finding, substantive Substantive) []apiv1.Finding {
	out := slices.Clone(findings)
	at := map[string]int{}
	for i, finding := range findings {
		if _, seen := at[FindingID(finding)]; !seen {
			at[FindingID(finding)] = i
		}
	}
	for _, finding := range extra {
		id := FindingID(finding)
		i, have := at[id]
		switch {
		case !have:
			at[id] = len(out)
			out = append(out, finding)
		case !allows(substantive, out[i]) && allows(substantive, finding):
			out[i] = finding
		}
	}
	return out
}

func allows(substantive Substantive, finding apiv1.Finding) bool {
	return substantive == nil || substantive(finding)
}

// ledger is the publication-order state machine Outstanding and Fold share.
type ledger struct {
	order []string
	open  map[string]apiv1.Finding
}

func replay(records []apiv1.Verdict, head string, substantive Substantive) *ledger {
	state := &ledger{open: map[string]apiv1.Finding{}}
	if head == "" {
		return state
	}
	for _, record := range records {
		if record.HeadSHA == head {
			state.apply(record, substantive)
		}
	}
	return state
}

func (l *ledger) apply(record apiv1.Verdict, substantive Substantive) {
	for _, disposition := range record.Dispositions {
		if disposition.Answers() {
			delete(l.open, strings.TrimSpace(disposition.FindingID))
		}
	}
	l.raise(record.StandingFindings, substantive)
	if record.Decision != apiv1.VerdictPass {
		l.raise(record.Findings, substantive)
	}
}

func (l *ledger) raise(findings []apiv1.Finding, substantive Substantive) {
	for _, finding := range findings {
		if !allows(substantive, finding) {
			continue
		}
		id := FindingID(finding)
		if _, ok := l.open[id]; !ok {
			l.order = append(l.order, id)
		}
		finding.ID = id
		l.open[id] = finding
	}
}

func (l *ledger) findings() []apiv1.Finding {
	var out []apiv1.Finding
	seen := map[string]bool{}
	for _, id := range l.order {
		finding, ok := l.open[id]
		if !ok || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, finding)
	}
	return out
}

func answeredIDs(dispositions []apiv1.FindingDisposition) map[string]bool {
	answered := map[string]bool{}
	for _, disposition := range dispositions {
		if disposition.Answers() {
			answered[strings.TrimSpace(disposition.FindingID)] = true
		}
	}
	return answered
}

// dispositionsOn returns every complete disposition records made on head, in
// publication order, one per finding id (the latest wins).
func dispositionsOn(records []apiv1.Verdict, head string) []apiv1.FindingDisposition {
	var out []apiv1.FindingDisposition
	for _, record := range records {
		if record.HeadSHA == head {
			out = mergeDispositions(out, record.Dispositions)
		}
	}
	return out
}

// mergeDispositions appends the complete dispositions in next to base, one
// per finding id, a later disposition replacing an earlier one in place.
func mergeDispositions(base, next []apiv1.FindingDisposition) []apiv1.FindingDisposition {
	out := slices.Clone(base)
	for _, disposition := range next {
		if !disposition.Answers() {
			continue
		}
		disposition.FindingID = strings.TrimSpace(disposition.FindingID)
		if i := slices.IndexFunc(out, func(d apiv1.FindingDisposition) bool { return d.FindingID == disposition.FindingID }); i >= 0 {
			out[i] = disposition
			continue
		}
		out = append(out, disposition)
	}
	return out
}
