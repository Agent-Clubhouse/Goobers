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
// support. Review threads are supported (ADO-N20): ListPullRequestReviewThreads
// is the embedded provider's own, no longer reported unsupported (#6134).
type mergeReviewADOProvider struct {
	*providers.ADOProvider
}

func (p mergeReviewADOProvider) RefCheckState(context.Context, providers.RepositoryRef, string) (providers.CheckState, error) {
	return providers.CheckState(""), p.unsupported("ref check state")
}

func (p mergeReviewADOProvider) RefCheckStates(context.Context, providers.RepositoryRef, []string) (map[string]providers.CheckState, error) {
	return nil, p.unsupported("ref check states")
}

// Every ID these stages pass is a pull-request number, which ADO would
// otherwise resolve as an unrelated Azure Boards work-item id. The overrides
// below route comment and label operations to the PR thread and PR label APIs.
func (p mergeReviewADOProvider) ListComments(ctx context.Context, repo providers.RepositoryRef, pullID string) ([]providers.Comment, error) {
	return p.ListPullRequestThreadComments(ctx, repo, pullID)
}

func (p mergeReviewADOProvider) UpdateComment(ctx context.Context, repo providers.RepositoryRef, commentID, body string) error {
	return p.UpdatePullRequestThreadComment(ctx, repo, commentID, body)
}

func (p mergeReviewADOProvider) DeleteComment(ctx context.Context, repo providers.RepositoryRef, commentID string) error {
	return p.DeletePullRequestThreadComment(ctx, repo, commentID)
}

func (p mergeReviewADOProvider) UpdateWorkItem(ctx context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	for _, name := range req.RemoveLabels {
		if err := p.RemovePullRequestLabel(ctx, req.Repository, req.ID, name); err != nil {
			return providers.WorkItem{}, err
		}
	}
	if len(req.AddLabels) > 0 {
		if err := p.AddPullRequestLabels(ctx, req.Repository, req.ID, req.AddLabels); err != nil {
			return providers.WorkItem{}, err
		}
	}
	if req.Comment != "" {
		if _, err := p.PostPullRequestThreadComment(ctx, req.Repository, req.ID, req.Comment); err != nil {
			return providers.WorkItem{}, err
		}
	}
	return providers.WorkItem{ID: req.ID}, nil
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

func (p mergeReviewADOProvider) ListRecentlyClosedPullRequests(context.Context, providers.ListPullRequestsRequest, time.Time) ([]providers.PullRequestSummary, error) {
	return nil, p.unsupported("recently closed pull requests")
}

func (p mergeReviewADOProvider) PullRequestMergeable(context.Context, providers.RepositoryRef, string) (*bool, error) {
	return nil, p.unsupported("pull request mergeability")
}

func (p mergeReviewADOProvider) RepositoryFileContent(context.Context, providers.RepositoryRef, string, string) ([]byte, error) {
	return nil, p.unsupported("repository file content")
}

func (p mergeReviewADOProvider) SubmitPullRequestReview(context.Context, providers.PullRequestReviewRequest) (providers.PullRequestReviewResult, error) {
	return providers.PullRequestReviewResult{}, p.unsupported("pull request review submission")
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
