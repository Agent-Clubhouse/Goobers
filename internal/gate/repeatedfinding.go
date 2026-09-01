package gate

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// ReasonFindingUnverifiedRepeat marks a needs-changes verdict whose every
// blocking finding already blocked the previous episode under the same
// identity AND names a location the latest authoritative diff does not
// contain (issue #3136). One stale or false observation repeated verbatim can
// otherwise consume every remaining repass, because each repetition looks
// like fresh work to do. Such a verdict routes to arbitration — the gate's
// escalate branch — instead of dispatching another implementer session that
// has no new evidence to act on.
const ReasonFindingUnverifiedRepeat = "REVIEW_FINDING_UNVERIFIED_REPEAT"

// RepeatedFindingArbitration is what the repeated-finding evidence check
// concluded about one verdict: which finding identities recurred from the
// previous episode, which of those the latest authoritative diff cannot
// corroborate, and whether the verdict must go to arbitration rather than
// charge another repass.
type RepeatedFindingArbitration struct {
	// Repeated are the identities this verdict raises that were also active
	// in the latest prior episode for the same gate.
	Repeated []string
	// Unverified are the Repeated identities whose Location is absent from
	// the latest authoritative diff, so the diff cannot corroborate them.
	Unverified []string
	// Arbitrate is true when every finding blocking this verdict is repeated
	// and none of them is verifiable against the diff.
	Arbitrate bool
}

// arbitrateRepeatedFindings checks a needs-changes verdict's findings against
// the previous episode's identities and the authoritative diff the reviewer
// was shown, and reports whether another repass can still be justified.
//
// Fail-open in exactly the way the other finding rules are (see
// ArtifactBytes): with no reachable episode history or no reachable diff,
// nothing is repeated, nothing is unverifiable, and the verdict routes
// normally. The check only ever withholds a repass a human then arbitrates —
// it never converts a needs-changes verdict into a pass.
func arbitrateRepeatedFindings(
	verdict apiv1.Verdict,
	pointers []apiv1.ContextPointer,
	resolve ArtifactBytes,
	gateName string,
) (apiv1.Verdict, RepeatedFindingArbitration) {
	var arbitration RepeatedFindingArbitration
	if verdict.Decision != apiv1.VerdictNeedsChanges || len(verdict.Findings) == 0 || resolve == nil {
		return verdict, arbitration
	}
	history := readEpisodeHistory(pointers, resolve, gateName)
	if len(history.active) == 0 {
		return verdict, arbitration
	}
	source := reviewerPatchSource(pointers, resolve, gateName)
	if len(source) == 0 {
		return verdict, arbitration
	}

	verified := false
	for _, finding := range verdict.Findings {
		if finding.ID == "" || history.active[finding.ID].ID == "" {
			return verdict, arbitration
		}
		arbitration.Repeated = append(arbitration.Repeated, finding.ID)
		if source.covers(finding.Location) {
			verified = true
			continue
		}
		arbitration.Unverified = append(arbitration.Unverified, finding.ID)
	}
	slices.Sort(arbitration.Repeated)
	slices.Sort(arbitration.Unverified)
	if verified {
		return verdict, arbitration
	}

	arbitration.Arbitrate = true
	note := fmt.Sprintf(
		"%s: every finding repeats the previous episode's identity (%s) and names a location absent from the reviewed diff, so another repass has no new evidence to act on — routing to arbitration",
		ReasonFindingUnverifiedRepeat, strings.Join(arbitration.Unverified, ", "),
	)
	if verdict.Rationale == "" {
		verdict.Rationale = note
	} else {
		verdict.Rationale += "\n\n" + note
	}
	return verdict, arbitration
}

// covers reports whether the authoritative diff contains the finding's
// location: the exact line when the location names one, and otherwise any
// line of the named file.
func (source patchSource) covers(location string) bool {
	location = strings.TrimSpace(location)
	if location == "" {
		return false
	}
	if _, ok := source.lineAt(location); ok {
		return true
	}
	path := location
	if colon := strings.LastIndexByte(location, ':'); colon > 0 {
		if _, err := strconv.Atoi(location[colon+1:]); err == nil {
			path = location[:colon]
		}
	}
	_, ok := source[path]
	return ok
}
