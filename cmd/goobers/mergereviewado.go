package main

import (
	"context"
	"fmt"
	"time"

	"github.com/goobers/goobers/providers"
)

// mergeReviewADOProvider adapts the ADO operations used by the merge-review
// election and refusal stages to the shared remediation helpers. Operations
// outside those stages remain unavailable rather than being mistaken for ADO
// support.
type mergeReviewADOProvider struct {
	*providers.ADOProvider
}

func (p mergeReviewADOProvider) RefCheckState(context.Context, providers.RepositoryRef, string) (providers.CheckState, error) {
	return providers.CheckState(""), p.unsupported("ref check state")
}

func (p mergeReviewADOProvider) RefCheckStates(context.Context, providers.RepositoryRef, []string) (map[string]providers.CheckState, error) {
	return nil, p.unsupported("ref check states")
}

func (p mergeReviewADOProvider) UpdateComment(context.Context, providers.RepositoryRef, string, string) error {
	return p.unsupported("comment update")
}

func (p mergeReviewADOProvider) DeleteComment(context.Context, providers.RepositoryRef, string) error {
	return p.unsupported("comment deletion")
}

func (p mergeReviewADOProvider) BranchTipSHA(context.Context, providers.RepositoryRef, string) (string, error) {
	return "", p.unsupported("branch tip")
}

func (p mergeReviewADOProvider) UpdateBranch(context.Context, providers.UpdateBranchRequest) (providers.UpdateBranchResult, error) {
	return providers.UpdateBranchResult{}, p.unsupported("branch update")
}

func (p mergeReviewADOProvider) CIFailures(context.Context, providers.RepositoryRef, string) ([]providers.CIFailureDetail, error) {
	return nil, p.unsupported("CI failures")
}

func (p mergeReviewADOProvider) ListPullRequestReviewThreads(context.Context, providers.RepositoryRef, string) (providers.PullRequestReviewThreads, error) {
	return providers.PullRequestReviewThreads{}, p.unsupported("review threads")
}

func (p mergeReviewADOProvider) ListRecentlyClosedPullRequests(context.Context, providers.ListPullRequestsRequest, time.Time) ([]providers.PullRequestSummary, error) {
	return nil, p.unsupported("recently closed pull requests")
}

func (p mergeReviewADOProvider) PullRequestMergeable(context.Context, providers.RepositoryRef, string) (*bool, error) {
	return nil, p.unsupported("pull request mergeability")
}

func (p mergeReviewADOProvider) unsupported(operation string) error {
	return fmt.Errorf("ADO merge-review does not support %s", operation)
}

func newMergeReviewRemediationProvider(root string, repo providers.RepositoryRef, opts ...stageProviderOption) (remediationProvider, error) {
	provider, err := newMergeReviewProvider(root, repo, false, opts...)
	if err != nil {
		return nil, err
	}
	if ado, ok := provider.(*providers.ADOProvider); ok {
		return mergeReviewADOProvider{ADOProvider: ado}, nil
	}
	remediation, ok := provider.(remediationProvider)
	if !ok {
		return nil, fmt.Errorf("repository provider %q does not support merge-review operations", repo.Provider)
	}
	return remediation, nil
}
