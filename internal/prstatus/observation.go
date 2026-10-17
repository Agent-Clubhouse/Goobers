package prstatus

import "github.com/goobers/goobers/providers"

// VerdictObservation describes what the existing live poll can establish. It
// is telemetry, not a merge decision: missing status remains allowed wherever
// VerdictStatusRefusal currently allows it.
type VerdictObservation string

const (
	VerdictPresent     VerdictObservation = "present"
	VerdictAbsent      VerdictObservation = "absent"
	VerdictUnsupported VerdictObservation = "unsupported"
	VerdictUnreadable  VerdictObservation = "unreadable"
)

// ObserveVerdict classifies an already-completed poll without another provider
// call. A provider's status publishing capability does not prove read support.
// GitHub and Gitea implement MergeReviewStatusReader and populate VerdictStatus
// from their poll; ADO currently does neither and must not count as absent.
func ObserveVerdict(provider any, poll providers.PullRequestPollResult, pollErr error) VerdictObservation {
	if pollErr != nil {
		return VerdictUnreadable
	}
	if poll.VerdictStatus != nil {
		return VerdictPresent
	}
	if _, ok := provider.(providers.MergeReviewStatusReader); !ok {
		return VerdictUnsupported
	}
	return VerdictAbsent
}

// WouldBlockMissingVerdict counts only attempts which passed every existing
// gate and reached landing. Opt-outs, advisory mode, failed checks, and unknown
// evidence are excluded rather than treated as missing-verdict false blocks.
func (o VerdictObservation) WouldBlockMissingVerdict(landingAttempted bool) bool {
	return o == VerdictAbsent && landingAttempted
}
