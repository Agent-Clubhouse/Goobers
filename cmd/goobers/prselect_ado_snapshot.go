package main

import (
	"context"
	"fmt"
	"strconv"

	webhookhttp "github.com/goobers/goobers/internal/webhook"
	"github.com/goobers/goobers/providers"
)

// selectionSnapshot uses the list's complete metadata, including the project
// identity needed for policy reads. Ineligible metadata never triggers a detail
// or policy request; every otherwise eligible PR still gets fresh checks so the
// complete fairness snapshot is preserved.
func (s branchPolicyPRSelectSource) selectionSnapshot(ctx context.Context, repo providers.RepositoryRef, req prSelectSourceRequest) ([]providers.PullRequestSummary, []providers.PullRequestSummary, error) {
	id, targeted := webhookhttp.PullNumberFromTriggerRef(req.triggerRef)
	if targeted && req.completeness != prSelectCompleteSnapshot {
		pr, err := adoSelectionCandidate(ctx, s.provider, repo, id)
		if err != nil {
			return nil, nil, fmt.Errorf("read webhook pull request #%s: %w", id, err)
		}
		matched, err := req.identityFilters.MatchesSummary(pr)
		if err != nil || !matched {
			return nil, nil, err
		}
		return []providers.PullRequestSummary{pr}, nil, nil
	}
	prs, err := s.snapshots.ListPullRequestsWithCheckFilter(ctx, providers.ListPullRequestsRequest{Repository: repo, Base: req.base}, func(pr providers.PullRequestSummary) (bool, error) {
		return adoSelectionNeedsChecks(pr, req)
	})
	if err != nil {
		return nil, nil, fmt.Errorf("list open pull requests: %w", err)
	}
	return pullRequestsForSelectionCommon(ctx, prs, req, selectionResolver{
		target: func(ctx context.Context, id string) (providers.PullRequestSummary, error) {
			for _, pr := range prs {
				if strconv.Itoa(pr.Number) == id {
					return pr, nil
				}
			}
			return adoSelectionCandidate(ctx, s.provider, repo, id)
		},
		resolve: func(context.Context, *providers.PullRequestSummary) error { return nil },
		listBase: func(ctx context.Context, base string) ([]providers.PullRequestSummary, error) {
			return s.snapshots.ListPullRequests(ctx, providers.ListPullRequestsRequest{Repository: repo, Base: base, SkipCheckState: true})
		},
	})
}

func adoSelectionNeedsChecks(pr providers.PullRequestSummary, req prSelectSourceRequest) (bool, error) {
	// Confirmed conflicts go directly to the remediation handoff, regardless of CI.
	if pr.Mergeable != nil && !*pr.Mergeable {
		return false, nil
	}
	if pr.State != "open" || pr.Draft || hasPRSelectExclusion(pr.Labels, req.excludeLabels) {
		return false, nil
	}
	if req.authorScope != authorScopeAny && !isOwnPullRequest(pr.Author, pr.Head, req.headPrefixes, req.expectedAuthorLogin) {
		return false, nil
	}
	if !eligibleByMergeReviewPolicy(pr, req.requiredOptInLabel, req.respectAssignee, req.selfIdentity) {
		return false, nil
	}
	return req.identityFilters.MatchesSummary(pr)
}
