package main

import (
	"context"
	"strconv"
	"strings"

	"github.com/goobers/goobers/providers"
)

// latestBlockedOnSiblingState scans comments (oldest first, ListComments' own
// order) for the LAST one carrying a blocked-on-sibling payload apply-verdict
// posted (#747) — only the most recently recorded block is still actionable —
// and returns that comment's ID too, so a caller can edit/clear it in place.
// found is false when no comment in the thread carries the payload (the PR was
// never parked as blocked-on-sibling), not an error. Mirrors
// latestRemediationState's shape (remediationcheckpoint.go).
func latestBlockedOnSiblingState(comments []providers.Comment) (state blockedOnSiblingState, commentID string, found bool) {
	for i := len(comments) - 1; i >= 0; i-- {
		if s, ok := parseBlockedOnSiblingComment(comments[i].Body); ok {
			return s, comments[i].ID, true
		}
	}
	return blockedOnSiblingState{}, "", false
}

// blockedOnSiblingStillBlocks reports whether pr's goobers:blocked-on-sibling
// parking still holds (#748): it carries the label, has a recorded set of
// named blocker PRs, and at least one of those blockers is still open. This is
// the blocker-aware analog of escalationStillBlocks — where that self-heals on
// "did my own SHAs move," this self-heals on "are all the specific PRs I'm
// waiting behind resolved," which is what blocked-on-sibling actually means
// (#747). A blocker is resolved when it is no longer open: merged (done) or
// closed without merging (nothing left to wait for) — both leave GitHub's
// issue state "closed", so a single open/closed check covers both.
//
// Fails OPEN (returns false, not blocked) for a PR labeled but carrying no
// recorded blocker set — a manual label, or a record post that never landed:
// there is no concrete condition to wait on, and a re-review will re-establish
// the record or clear the label, so parking forever on an unknowable state is
// the wrong default (contrast escalationStillBlocks, which fails closed because
// its snapshot self-heal has a well-defined "SHAs unchanged" fallback).
//
// Like escalationStillBlocks, it only fetches comments/blocker states for PRs
// that actually carry the label — a small by-design subset — so it costs
// nothing for the common unlabeled candidate.
func blockedOnSiblingStillBlocks(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, pr providers.PullRequestSummary) (bool, error) {
	if !hasAnyLabel(pr.Labels, []string{blockedOnSiblingLabel}) {
		return false, nil
	}
	comments, err := provider.ListComments(ctx, repo, strconv.Itoa(pr.Number))
	if err != nil {
		return false, err
	}
	state, _, found := latestBlockedOnSiblingState(comments)
	if !found || len(state.Blockers) == 0 {
		return false, nil
	}
	for _, blocker := range state.Blockers {
		item, err := provider.GetWorkItem(ctx, repo, strconv.Itoa(blocker))
		if err != nil {
			return false, err
		}
		if strings.EqualFold(item.State, "open") {
			return true, nil
		}
	}
	return false, nil
}
