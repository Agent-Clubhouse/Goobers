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

// recordedBlockedOnSiblingBlockers fails open for an absent record: without
// named blockers there is no concrete condition that can keep a PR parked.
func recordedBlockedOnSiblingBlockers(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, pr providers.PullRequestSummary) ([]int, error) {
	if !hasAnyLabel(pr.Labels, []string{blockedOnSiblingLabel}) {
		return nil, nil
	}
	comments, err := provider.ListComments(ctx, repo, strconv.Itoa(pr.Number))
	if err != nil {
		return nil, err
	}
	state, _, found := latestBlockedOnSiblingState(comments)
	if !found || len(state.Blockers) == 0 {
		return nil, nil
	}
	return state.Blockers, nil
}

// liveBlockedOnSiblingBlockers returns the named blocker PRs that are still
// open for a parked PR. A blocker is resolved when it is no longer open:
// merged (done) or closed without merging (nothing left to wait for).
func liveBlockedOnSiblingBlockers(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, pr providers.PullRequestSummary) ([]int, error) {
	blockers, err := recordedBlockedOnSiblingBlockers(ctx, provider, repo, pr)
	if err != nil {
		return nil, err
	}
	var open []int
	seen := make(map[int]bool)
	for _, blocker := range blockers {
		if seen[blocker] {
			continue
		}
		seen[blocker] = true
		item, err := provider.GetWorkItem(ctx, repo, strconv.Itoa(blocker))
		if err != nil {
			return nil, err
		}
		if strings.EqualFold(item.State, "open") {
			open = append(open, blocker)
		}
	}
	return open, nil
}

// blockedOnSiblingStillBlocks reports whether pr's blocker-aware parking still
// holds (#748). It is also used by post-merge unpark and pr-remediation.
func blockedOnSiblingStillBlocks(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, pr providers.PullRequestSummary) (bool, error) {
	blockers, err := recordedBlockedOnSiblingBlockers(ctx, provider, repo, pr)
	if err != nil {
		return false, err
	}
	for _, blocker := range blockers {
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
