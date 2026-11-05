package remediationselect

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/providers"
)

// PullRequestBehindLiveBase reports whether pr's merge base with the live tip of its base branch is behind that tip, caching tips per base in baseTips.
func PullRequestBehindLiveBase(ctx context.Context, provider Provider, repo providers.RepositoryRef, pr providers.PullRequestSummary, baseTips map[string]string) (bool, error) {
	mergeBase, baseTip, err := PullRequestLiveBaseMergeBase(ctx, provider, repo, pr, baseTips)
	if err != nil {
		return false, err
	}
	return mergeBase != baseTip, nil
}

// PullRequestLiveBaseMergeBase resolves pr's merge-base with the live tip of
// its base branch, caching the tip per base in baseTips.
func PullRequestLiveBaseMergeBase(ctx context.Context, provider Provider, repo providers.RepositoryRef, pr providers.PullRequestSummary, baseTips map[string]string) (mergeBase, baseTip string, err error) {
	baseTip = baseTips[pr.Base]
	if baseTip == "" {
		baseTip, err = provider.BranchTipSHA(ctx, repo, pr.Base)
		if err != nil {
			return "", "", fmt.Errorf("resolve live base branch %q: %w", pr.Base, err)
		}
		baseTips[pr.Base] = baseTip
	}
	compared, err := provider.CompareCommits(ctx, repo, baseTip, pr.HeadSHA)
	if err != nil {
		return "", "", fmt.Errorf("compare live base with PR #%d head: %w", pr.Number, err)
	}
	if compared.MergeBaseSHA == "" {
		return "", "", fmt.Errorf("compare live base with PR #%d head returned no merge base", pr.Number)
	}
	return compared.MergeBaseSHA, baseTip, nil
}
