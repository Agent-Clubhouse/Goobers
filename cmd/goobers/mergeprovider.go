package main

import (
	"context"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

// mergeProvider is the forge surface merge-pr uses directly: its own
// pre-merge reads (PollPullRequest for live head/base + CI state,
// CompareCommits for the delta-aware SHA-pin check, PullRequestFiles for
// base-movement intersection) plus the post-merge branch cleanup
// (ListPullRequests to detect stacked children, DeleteBranch to remove the
// merged head).
//
// The capability-gated landing calls (MergePullRequest, DetectMergePolicy,
// EnqueuePullRequest) are NOT here: those go through providers.Dispatcher,
// which refuses an undeclared capability before the backend sees the call.
// Dispatcher takes providers.Provider, which both backends satisfy.
type mergeProvider interface {
	providers.Provider
	providers.CommitComparer
	providers.BranchDeleter
	// AuthenticatedLogin lets a mergeProvider satisfy remediationProvider, which
	// classifyRemoteTutorChanges (reached from merge-pr's tutor-branch path)
	// takes. Both backends implement it.
	AuthenticatedLogin(ctx context.Context) (string, error)
	ListComments(ctx context.Context, repo providers.RepositoryRef, id string) ([]providers.Comment, error)
	UpdateComment(ctx context.Context, repo providers.RepositoryRef, commentID, body string) error
	DeleteComment(ctx context.Context, repo providers.RepositoryRef, commentID string) error
	GetPullRequest(ctx context.Context, repo providers.RepositoryRef, pullID string) (providers.PullRequestSummary, error)
	GetWorkItem(ctx context.Context, repo providers.RepositoryRef, id string) (providers.WorkItem, error)
	UpdateWorkItem(ctx context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error)
	UpdateWorkItemStatus(ctx context.Context, req providers.UpdateWorkItemStatusRequest) (providers.WorkItem, error)
	RepositoryFileContent(ctx context.Context, repo providers.RepositoryRef, path, ref string) ([]byte, error)
	PullRequestMergeable(ctx context.Context, repo providers.RepositoryRef, pullID string) (*bool, error)
	RefCheckState(ctx context.Context, repo providers.RepositoryRef, ref string) (providers.CheckState, error)
	RefCheckStates(ctx context.Context, repo providers.RepositoryRef, refs []string) (map[string]providers.CheckState, error)
	ListRecentlyClosedPullRequests(ctx context.Context, req providers.ListPullRequestsRequest, updatedSince time.Time) ([]providers.PullRequestSummary, error)
	BranchTipSHA(ctx context.Context, repo providers.RepositoryRef, branch string) (string, error)
	UpdateBranch(ctx context.Context, req providers.UpdateBranchRequest) (providers.UpdateBranchResult, error)
	CIFailures(ctx context.Context, repo providers.RepositoryRef, ref string) ([]providers.CIFailureDetail, error)
	ListPullRequestReviewThreads(ctx context.Context, repo providers.RepositoryRef, pullID string) (providers.PullRequestReviewThreads, error)
	SubmitPullRequestReview(ctx context.Context, req providers.PullRequestReviewRequest) (providers.PullRequestReviewResult, error)
}

// Compile-time proof both backends satisfy the surface.
var (
	_ mergeProvider = (*providers.GitHubProvider)(nil)
	_ mergeProvider = (*providers.GiteaProvider)(nil)
)

// mergeStageProviderWithRecorder builds post-merge-reconcile's merge/unpark
// forge client through the shared merge-review/stage provider seam, with an
// explicit journal mutation recorder. The post-merge branch delete must record
// kind="branch", not kind="pr": the run journal's mutation facts are how an
// operator tells a merge from the branch cleanup that followed it, and
// collapsing both onto the PR recorder loses that distinction — which is why
// the recorder is passed in rather than derived from a kind here.
//
// It used to hand-roll its arms, and its GitHub arm called
// newCachedGitHubProvider directly. That skipped newGitHubProviderForStage and
// therefore skipped providers.WithConfiguredLogin, so under GitHub App auth the
// provider had no declared identity and AuthenticatedLogin — which
// post-merge-reconcile reaches through reconcileOpenPullRequestParks' sibling
// unpark checks — fell back to GET /user. Installation tokens cannot call that
// endpoint, so reconciliation died with "Resource not accessible by
// integration" and silently stopped unparking (#3890). #3885/#3886 fixed
// apply-verdict's identical constructor but missed this sibling; routing both
// through the seam is what keeps them from drifting apart again.
//
// Per-arm behavior is preserved exactly: the GitHub arm stays conditional-GET
// cached, the Gitea arm keeps its recorder and stays uncached, and both keep
// using the caller's own capability token. ADO now reaches the registered ADO
// factory instead of the old `default:` arm, which silently handed an ADO repo
// a GitHub provider; post-merge-reconcile routes ADO to its own command before
// this constructor, so no live path changes.
func mergeStageProviderWithRecorder(root string, repo providers.RepositoryRef, token string, recorder providers.MutationRecorder) (mergeProvider, error) {
	opts := []stageProviderOption{
		withStageProviderCapability(capability.GitHubPRWrite),
		withStageProviderToken(token),
		withStageProviderMutationRecorder(recorder),
	}
	if repo.Provider == providers.ProviderGitHub {
		// The conditional-GET read cache is a GitHub HTTPClient decorator
		// (apireadcache.go); the Gitea arm has never been cached.
		opts = append(opts, withStageProviderCache())
	}
	return newMergeReviewProviderAs[mergeProvider](root, repo, false, opts...)
}
