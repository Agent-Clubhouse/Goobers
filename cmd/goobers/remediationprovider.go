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
	RepositoryFileContent(ctx context.Context, repo providers.RepositoryRef, path, ref string) ([]byte, error)
	ListComments(ctx context.Context, repo providers.RepositoryRef, id string) ([]providers.Comment, error)
	UpdateComment(ctx context.Context, repo providers.RepositoryRef, commentID, body string) error
	DeleteComment(ctx context.Context, repo providers.RepositoryRef, commentID string) error
	AuthenticatedLogin(ctx context.Context) (string, error)
	SubmitPullRequestReview(ctx context.Context, req providers.PullRequestReviewRequest) (providers.PullRequestReviewResult, error)
	ListWorkItems(ctx context.Context, req providers.ListWorkItemsRequest) ([]providers.WorkItem, error)
	GetWorkItem(ctx context.Context, repo providers.RepositoryRef, id string) (providers.WorkItem, error)
	CreateWorkItem(ctx context.Context, req providers.CreateWorkItemRequest) (providers.WorkItem, error)
	UpdateWorkItem(ctx context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error)
	UpdateWorkItemStatus(ctx context.Context, req providers.UpdateWorkItemStatusRequest) (providers.WorkItem, error)
	ClaimWorkItem(ctx context.Context, req providers.ClaimWorkItemRequest) (providers.ClaimResult, error)
	BranchTipSHA(ctx context.Context, repo providers.RepositoryRef, branch string) (string, error)
	CompareCommits(ctx context.Context, repo providers.RepositoryRef, base, head string) (providers.CompareResult, error)
	PullRequestMergeable(ctx context.Context, repo providers.RepositoryRef, pullID string) (*bool, error)
	PollPullRequest(ctx context.Context, req providers.PullRequestPollRequest) (providers.PullRequestPollResult, error)
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
	return remediationStageProviderWithRecorder(root, repo, token, cached, nil)
}

// remediationStageProviderWithRecorder is remediationStageProvider plus a
// journal mutation recorder wired to whichever backend the routed repo
// selects. A mutating stage (post-merge's sibling triage and issue close-out,
// merge-pr's branch cleanup) must record its external refs on either forge,
// so the recorder cannot live on the GitHub arm alone.
func remediationStageProviderWithRecorder(root string, repo providers.RepositoryRef, token string, cached bool, recorder providers.MutationRecorder) (remediationProvider, error) {
	switch repo.Provider {
	case providers.ProviderGitea, providers.ProviderGitHub:
		// Through the shared stage-provider seam, not a backend constructor:
		// this lane's surface includes AuthenticatedLogin, so a provider built
		// off-seam carries no declared identity — #3885/#3890 on the local
		// substrate, and #3914 in a pod, where the login is stamped run
		// identity because there is no instance config to read.
		opts := []stageProviderOption{withStageProviderToken(token)}
		if recorder != nil {
			opts = append(opts, withStageProviderMutationRecorder(recorder))
		}
		// The conditional-GET read cache is a GitHub HTTPClient decorator
		// (apireadcache.go); the Gitea arm has never been cached.
		if cached && repo.Provider == providers.ProviderGitHub {
			opts = append(opts, withStageProviderCache())
		}
		return newProviderForStageSurface[remediationProvider](root, repo, false, opts...)
	default:
		return nil, fmt.Errorf("pr-remediation does not support repository provider %q", repo.Provider)
	}
}
