package main

import (
	"context"
	"fmt"
	"time"

	"github.com/goobers/goobers/providers"
)

// remediationProvider is the narrow surface the pr-remediation lane needs.
// Both *providers.GitHubProvider and
// *providers.GiteaProvider satisfy it, so the lane is provider-neutral once
// the backend is resolved from the routed repo — same idiom as
// openPRProvider (openpr.go).
type remediationProvider interface {
	ListPullRequests(ctx context.Context, req providers.ListPullRequestsRequest) ([]providers.PullRequestSummary, error)
	ListRecentlyClosedPullRequests(ctx context.Context, req providers.ListPullRequestsRequest, updatedSince time.Time) ([]providers.PullRequestSummary, error)
	RefCheckState(ctx context.Context, repo providers.RepositoryRef, ref string) (providers.CheckState, error)
	RefCheckStates(ctx context.Context, repo providers.RepositoryRef, refs []string) (map[string]providers.CheckState, error)
	GetPullRequest(ctx context.Context, repo providers.RepositoryRef, pullID string) (providers.PullRequestSummary, error)
	PullRequestFiles(ctx context.Context, repo providers.RepositoryRef, pullID string) ([]providers.ChangedFile, error)
	ListComments(ctx context.Context, repo providers.RepositoryRef, id string) ([]providers.Comment, error)
	UpdateComment(ctx context.Context, repo providers.RepositoryRef, commentID, body string) error
	DeleteComment(ctx context.Context, repo providers.RepositoryRef, commentID string) error
	AuthenticatedLogin(ctx context.Context) (string, error)
	GetWorkItem(ctx context.Context, repo providers.RepositoryRef, id string) (providers.WorkItem, error)
	UpdateWorkItem(ctx context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error)
	BranchTipSHA(ctx context.Context, repo providers.RepositoryRef, branch string) (string, error)
	CompareCommits(ctx context.Context, repo providers.RepositoryRef, base, head string) (providers.CompareResult, error)
	PullRequestMergeable(ctx context.Context, repo providers.RepositoryRef, pullID string) (*bool, error)
	UpdateBranch(ctx context.Context, req providers.UpdateBranchRequest) (providers.UpdateBranchResult, error)
	CIFailures(ctx context.Context, repo providers.RepositoryRef, ref string) ([]providers.CIFailureDetail, error)
	ListPullRequestReviewThreads(ctx context.Context, repo providers.RepositoryRef, pullID string) (providers.PullRequestReviewThreads, error)
}

// Compile-time contract: both concrete backends satisfy the lane surface.
var (
	_ remediationProvider = (*providers.GitHubProvider)(nil)
	_ remediationProvider = (*providers.GiteaProvider)(nil)
)

// remediationStageProvider builds the provider a pr-remediation stage talks
// to, dispatched by the routed repo's kind — the openpr.go per-kind idiom
// (github | gitea | default-error). ADO is default-error: it declares
// neither pr.review.threads nor the CI/branch-tip read surfaces this lane
// needs, and CONF-6's preflight refuses it before a run ever starts.
// token is the stage's own capability-scoped credential (providerToken);
// cached selects the conditional-GET read cache on the GitHub arm only —
// the cache is a GitHub HTTPClient decorator (apireadcache.go) and the
// Gitea arm stays uncached, exactly like open-pr's and backlog-query's
// Gitea arms today.
var remediationStageProvider = buildRemediationStageProvider

func buildRemediationStageProvider(root string, repo providers.RepositoryRef, token string, cached bool) (remediationProvider, error) {
	if repo.Provider == providers.ProviderADO {
		return nil, fmt.Errorf("pr-remediation does not support repository provider %q", repo.Provider)
	}
	opts := []stageProviderOption{withStageProviderToken(token)}
	if cached {
		opts = append(opts, withStageProviderCache())
	}
	provider, err := newProviderForStage(root, repo, true, opts...)
	if err != nil {
		return nil, err
	}
	remediation, ok := provider.(remediationProvider)
	if !ok {
		return nil, fmt.Errorf("pr-remediation does not support repository provider %q", repo.Provider)
	}
	return remediation, nil
}
