package prstatus

import (
	"fmt"
	"regexp"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// The merge-review verdict status (#4199) is the native, merge-blocking form
// of merge-review's verdict: apply-verdict publishes it on the exact head it
// reviewed, and repositories make its context a required status check so
// no merge path (a human, native auto-merge, Goobers' own merge-pr) can land
// a pull request the authoritative reviewer has not passed.
//
// The status is pinned to the head by construction, because a commit status
// belongs to one commit: a verdict for an older head never reaches a newer
// one, which carries no verdict status until it is reviewed. The base is
// pinned through the description, which always begins
// "<decision> for base <sha>"; merge-pr refuses a pass recorded for any base
// but the one its own review pinned, and apply-verdict replaces the status
// with a pending "stale" one when it finds the base has moved under a
// standing verdict.

// verdictDescriptionPattern recovers the decision and reviewed base from a
// verdict status description.
var verdictDescriptionPattern = regexp.MustCompile(`^(\S+) for base (\S+)`)

// VerdictState maps a published verdict decision to the status state that
// enforces it: pass succeeds; needs-changes, fail and escalate fail; defer
// (waiting on a sibling, not a defect) stays pending. Every state but
// success blocks a required check.
func VerdictState(decision apiv1.VerdictDecision) (providers.CheckState, error) {
	switch decision {
	case apiv1.VerdictPass:
		return providers.CheckStatePassing, nil
	case apiv1.VerdictNeedsChanges, apiv1.VerdictFail, apiv1.VerdictEscalate:
		return providers.CheckStateFailing, nil
	case apiv1.VerdictDefer:
		return providers.CheckStatePending, nil
	default:
		return "", fmt.Errorf("unsupported verdict decision %q", decision)
	}
}

// VerdictDescription renders the status description: the decision and the
// reviewed base, then a one-line summary. Providers bound its length.
func VerdictDescription(decision apiv1.VerdictDecision, baseSHA, summary string) string {
	description := fmt.Sprintf("%s for base %s", decision, strings.TrimSpace(baseSHA))
	if line := strings.Join(strings.Fields(summary), " "); line != "" {
		description += ": " + line
	}
	return description
}

// VerdictStatusRequest builds the status publishing decision for the pull
// request at headSHA, reviewed against baseSHA.
func VerdictStatusRequest(
	repo providers.RepositoryRef,
	pullID string,
	decision apiv1.VerdictDecision,
	headSHA, baseSHA, summary, targetURL string,
) (providers.PullRequestStatusRequest, error) {
	state, err := VerdictState(decision)
	if err != nil {
		return providers.PullRequestStatusRequest{}, err
	}
	if strings.TrimSpace(headSHA) == "" || strings.TrimSpace(baseSHA) == "" {
		return providers.PullRequestStatusRequest{}, fmt.Errorf("a verdict status needs the reviewed head and base")
	}
	return providers.PullRequestStatusRequest{
		Repository:  repo,
		PullID:      pullID,
		Genre:       providers.MergeReviewStatusGenre,
		Name:        providers.MergeReviewStatusName,
		State:       state,
		Description: VerdictDescription(decision, baseSHA, summary),
		TargetURL:   targetURL,
		HeadSHA:     headSHA,
	}, nil
}

// StaleVerdictStatusRequest builds the pending status that withdraws a
// standing verdict on headSHA once the review it recorded no longer stands
// (the base moved, or the reviewer's pin disagreed). Pending blocks a
// required check until merge-review publishes a fresh verdict.
func StaleVerdictStatusRequest(repo providers.RepositoryRef, pullID, headSHA, baseSHA, reason, targetURL string) providers.PullRequestStatusRequest {
	return providers.PullRequestStatusRequest{
		Repository:  repo,
		PullID:      pullID,
		Genre:       providers.MergeReviewStatusGenre,
		Name:        providers.MergeReviewStatusName,
		State:       providers.CheckStatePending,
		Description: VerdictDescription("stale", baseSHA, "re-review pending; "+reason),
		TargetURL:   targetURL,
		HeadSHA:     headSHA,
	}
}

// ParseVerdictDescription recovers the decision and reviewed base a verdict
// status description records.
func ParseVerdictDescription(description string) (decision, baseSHA string, ok bool) {
	match := verdictDescriptionPattern.FindStringSubmatch(strings.TrimSpace(description))
	if match == nil {
		return "", "", false
	}
	return match[1], strings.TrimSuffix(match[2], ":"), true
}

// VerdictStatusRefusal returns why the merge-review verdict status on the
// polled head forbids landing a review pinned to baseSHA, or "" when it
// permits it. A head without the status is not refused here: the provider's
// required check refuses that natively, and a credential that cannot publish
// statuses must not stop merge-pr, whose other conjuncts still hold.
func VerdictStatusRefusal(status *providers.CheckDetail, headSHA, baseSHA string) string {
	if status == nil {
		return ""
	}
	prefix := fmt.Sprintf("merge-review verdict status %s on head %s", providers.MergeReviewStatusContext, headSHA)
	switch status.State {
	case providers.CheckStatePassing:
		_, base, ok := ParseVerdictDescription(status.Summary)
		if !ok {
			return prefix + " is a pass that names no reviewed base — not an authoritative verdict (#4199)"
		}
		if base != baseSHA {
			return fmt.Sprintf("%s is a pass for base %s, but this landing is pinned to base %s — not the current verdict (#4199)", prefix, base, baseSHA)
		}
		return ""
	case providers.CheckStateFailing:
		return fmt.Sprintf("%s is failure (%s) — a non-pass merge-review verdict blocks landing (#4199)", prefix, status.Summary)
	default:
		return fmt.Sprintf("%s is pending (%s) — no current pass verdict (#4199)", prefix, status.Summary)
	}
}
