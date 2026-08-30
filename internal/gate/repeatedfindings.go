package gate

import (
	"fmt"
	"slices"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// Repeat dispositions are the two answers arbitrateRepeatedFindings can give
// about a finding the reviewer already raised before this repass, journaled so
// an operator can tell "the reviewer's repeat is corroborated by the change
// under review" from "the reviewer repeated an observation the change under
// review cannot confirm" without re-reading the diff by hand.
const (
	// RepeatDispositionDispatch means the repeated finding still resolves to a
	// line of the current authoritative diff, so another repass is warranted.
	RepeatDispositionDispatch = "dispatch"
	// RepeatDispositionArbitration means the repeated finding cannot be
	// verified against the current authoritative diff, so the run stops for
	// arbitration instead of spending another implementer session on it.
	RepeatDispositionArbitration = "arbitration"
)

// ReasonFindingArbitration marks a needs-changes verdict whose findings were
// ALL repeats of the previous episode's that the current authoritative diff
// cannot corroborate. The run routes to escalation for arbitration rather than
// dispatching a repass that can only re-observe the same disagreement (issue
// #3136).
const ReasonFindingArbitration = "REVIEW_FINDING_ARBITRATION"

// RepeatFindingDisposition is one repeated finding's routing record: why this
// evaluation dispatched another repass for it, or why it stopped for
// arbitration instead.
type RepeatFindingDisposition struct {
	FindingID   string `json:"findingId"`
	Location    string `json:"location,omitempty"`
	Disposition string `json:"disposition"`
	Detail      string `json:"detail"`
}

// RepeatArbitration is what one arbitration pass concluded: the identities it
// could not verify, the per-finding explanations it journals, and whether the
// verdict as a whole must route to arbitration.
type RepeatArbitration struct {
	// Arbitrated lists the repeated finding identities the current
	// authoritative diff cannot corroborate.
	Arbitrated []string
	// Dispositions explains every REPEATED finding — dispatched and
	// arbitrated alike. A finding raised for the first time is absent: it has
	// consumed no repass yet, so there is nothing to explain.
	Dispositions []RepeatFindingDisposition
	// Arbitrate is true when every finding left on the verdict is a repeat
	// the diff cannot corroborate.
	Arbitrate bool
}

// arbitrateRepeatedFindings checks the findings a reviewer raised AGAIN after a
// repass against the diff that repass actually produced (issue #3136).
//
// Without it, a reviewer that repeats substantially the same finding every
// round spends the whole bounded repass budget on implementer sessions that
// can add no information: the finding-lifecycle rules in learningfindings.go
// only suppress an identity a PREVIOUS episode already resolved, and
// disproveReviewerFindings only removes the one narrow class of finding
// deterministic source evidence can refute outright. A repeat that is neither
// — the reviewer restating an observation the change under review does not
// contain — is dispatched again, unexamined, until the budget runs out.
//
// The check is deliberately narrow, because the failure mode it must never
// have is suppressing real work:
//
//   - Only a finding whose identity was ACTIVE in the latest injected episode
//     is considered. A first observation is always dispatched.
//   - A repeat whose Location resolves to a line of the gate's own authoritative
//     diff is corroborated: the reviewer is pointing at the change under review,
//     so another repass is the right answer and the disposition says so.
//   - A repeat whose Location is absent from that diff — or that carries no
//     location at all — is unverifiable, and routes to arbitration.
//   - Arbitration only fires when EVERY remaining finding is such a repeat. One
//     corroborated or first-time finding is enough work to justify the repass,
//     and the unverifiable repeats ride along with it (still journaled).
//
// Like every other rule on this seam, an unreachable resolver or an
// unreachable diff means "no evidence", and no evidence never arbitrates.
func arbitrateRepeatedFindings(
	verdict apiv1.Verdict,
	pointers []apiv1.ContextPointer,
	resolve ArtifactBytes,
	gateName string,
) (apiv1.Verdict, RepeatArbitration) {
	var arbitration RepeatArbitration
	if len(verdict.Findings) == 0 || resolve == nil {
		return verdict, arbitration
	}
	source := reviewerPatchSource(pointers, resolve, gateName)
	if len(source) == 0 {
		return verdict, arbitration
	}
	history := readEpisodeHistory(pointers, resolve, gateName)
	if len(history.active) == 0 {
		return verdict, arbitration
	}

	unverifiable := 0
	for _, finding := range verdict.Findings {
		if finding.ID == "" || history.active[finding.ID].ID == "" {
			continue
		}
		if _, ok := source.lineAt(finding.Location); ok {
			arbitration.Dispositions = append(arbitration.Dispositions, RepeatFindingDisposition{
				FindingID:   finding.ID,
				Location:    finding.Location,
				Disposition: RepeatDispositionDispatch,
				Detail: fmt.Sprintf(
					"repeated finding is corroborated by %s.diff at %s",
					gateName, finding.Location,
				),
			})
			continue
		}
		unverifiable++
		arbitration.Arbitrated = append(arbitration.Arbitrated, finding.ID)
		arbitration.Dispositions = append(arbitration.Dispositions, RepeatFindingDisposition{
			FindingID:   finding.ID,
			Location:    finding.Location,
			Disposition: RepeatDispositionArbitration,
			Detail:      unverifiableDetail(gateName, finding.Location),
		})
	}
	arbitration.Arbitrate = unverifiable > 0 && unverifiable == len(verdict.Findings)
	slices.Sort(arbitration.Arbitrated)
	if !arbitration.Arbitrate {
		return verdict, arbitration
	}

	details := make([]string, 0, len(arbitration.Dispositions))
	for _, disposition := range arbitration.Dispositions {
		if disposition.Disposition == RepeatDispositionArbitration {
			details = append(details, disposition.Detail)
		}
	}
	note := ReasonFindingArbitration + ": " + strings.Join(details, "; ")
	if verdict.Rationale == "" {
		verdict.Rationale = note
	} else {
		verdict.Rationale += "\n\n" + note
	}
	return verdict, arbitration
}

func unverifiableDetail(gateName, location string) string {
	if strings.TrimSpace(location) == "" {
		return fmt.Sprintf("repeated finding carries no location to verify against %s.diff", gateName)
	}
	return fmt.Sprintf("repeated finding cites %s, which is absent from %s.diff", location, gateName)
}
