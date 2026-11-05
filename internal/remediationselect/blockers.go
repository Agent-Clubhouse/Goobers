package remediationselect

import (
	"context"
	"strconv"
	"strings"

	"github.com/goobers/goobers/providers"
)

// LatestBlockedOnSiblingState scans comments (oldest first, ListComments' own
// order) for the LAST one carrying a blocked-on-sibling payload apply-verdict
// posted (#747) — only the most recently recorded block is still actionable —
// and returns that comment's ID too, so a caller can edit/clear it in place.
// found is false when no comment in the thread carries the payload (the PR was
// never parked as blocked-on-sibling), not an error. Mirrors
// latestRemediationState's shape (remediationcheckpoint.go).
func LatestBlockedOnSiblingState(comments []providers.Comment) (state BlockedOnSiblingState, commentID string, found bool) {
	for i := len(comments) - 1; i >= 0; i-- {
		if s, ok := ParseBlockedOnSiblingComment(comments[i].Body); ok {
			return s, comments[i].ID, true
		}
	}
	return BlockedOnSiblingState{}, "", false
}

// NamedBlockerStillBlocks reports whether a sibling PR or issue named as a blocker is still in flight: open, and not under a merge demotion that currently lets its successors drain.
func NamedBlockerStillBlocks(ctx context.Context, provider Provider, repo providers.RepositoryRef, blocker int) (bool, error) {
	item, err := provider.GetWorkItem(ctx, repo, strconv.Itoa(blocker))
	if err != nil {
		return false, err
	}
	if !strings.EqualFold(item.State, "open") {
		return false, nil
	}
	if !item.HasLabel(MergeDemotedLabel) {
		return true, nil
	}
	pr, err := provider.GetPullRequest(ctx, repo, strconv.Itoa(blocker))
	if err != nil {
		return false, err
	}
	demoted, err := DemotionStillHolds(ctx, provider, repo, pr)
	if err != nil {
		return false, err
	}
	// A live demotion lets successors drain; a stale label does not.
	return !demoted, nil
}

// LiveBlockedOnSiblingState is a parked PR's recorded blocked-on-sibling state with Blockers narrowed to those still effective.
func LiveBlockedOnSiblingState(ctx context.Context, provider Provider, repo providers.RepositoryRef, pr providers.PullRequestSummary) (BlockedOnSiblingState, error) {
	if !HasAnyLabel(pr.Labels, []string{BlockedOnSiblingLabel}) {
		return BlockedOnSiblingState{}, nil
	}
	comments, err := provider.ListComments(ctx, repo, strconv.Itoa(pr.Number))
	if err != nil {
		return BlockedOnSiblingState{}, err
	}
	state, _, found := LatestBlockedOnSiblingState(comments)
	if !found {
		return BlockedOnSiblingState{}, nil
	}
	state.Blockers, err = FilterLiveBlockedOnSiblingBlockers(ctx, provider, repo, state.Blockers)
	if err != nil {
		return BlockedOnSiblingState{}, err
	}
	return state, nil
}

// FilterLiveBlockedOnSiblingBlockers returns the distinct blockers that still block, in first-seen order, stopping at the first lookup failure.
func FilterLiveBlockedOnSiblingBlockers(ctx context.Context, provider Provider, repo providers.RepositoryRef, blockers []int) ([]int, error) {
	var open []int
	seen := make(map[int]bool)
	for _, blocker := range blockers {
		if seen[blocker] {
			continue
		}
		seen[blocker] = true
		blocks, err := NamedBlockerStillBlocks(ctx, provider, repo, blocker)
		if err != nil {
			return nil, err
		}
		if blocks {
			open = append(open, blocker)
		}
	}
	return open, nil
}
