package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

func remediationPullRequests(
	ctx context.Context,
	provider remediationProvider,
	repo providers.RepositoryRef,
	base, headPrefix string,
	target remediationTarget,
) ([]providers.PullRequestSummary, error) {
	return remediationPullRequestCandidates(ctx, repo, base, headPrefix, target, provider.ListPullRequests, func(ctx context.Context, id string) (providers.PullRequestSummary, error) {
		return provider.GetPullRequest(ctx, repo, id)
	})
}

func remediationPullRequestCandidates(
	ctx context.Context,
	repo providers.RepositoryRef,
	base, headPrefix string,
	target remediationTarget,
	list func(context.Context, providers.ListPullRequestsRequest) ([]providers.PullRequestSummary, error),
	get func(context.Context, string) (providers.PullRequestSummary, error),
) ([]providers.PullRequestSummary, error) {
	prs, err := list(ctx, providers.ListPullRequestsRequest{
		Repository: repo, Base: base, HeadPrefix: headPrefix, SkipCheckState: true,
	})
	if err != nil || !target.targeted {
		return prs, err
	}
	filtered := prs[:0]
	for _, candidate := range prs {
		if candidate.Number != target.number {
			filtered = append(filtered, candidate)
		}
	}
	pr, err := get(ctx, fmt.Sprint(target.number))
	if err != nil {
		if providers.IsNotFoundError(err) {
			return filtered, nil
		}
		return nil, fmt.Errorf("read targeted PR #%d: %w", target.number, err)
	}
	if pr.Number != target.number {
		return nil, fmt.Errorf("targeted PR lookup returned #%d, want #%d", pr.Number, target.number)
	}
	if pr.Merged || !strings.EqualFold(pr.State, "open") || (base != "" && pr.Base != base) {
		return filtered, nil
	}
	return append(filtered, pr), nil
}

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
	ClosePullRequest(ctx context.Context, req providers.ClosePullRequestRequest) (providers.ClosePullRequestResult, error)
}

// Compile-time contract: both concrete backends satisfy the lane surface.
var (
	_ remediationProvider = (*providers.GitHubProvider)(nil)
	_ remediationProvider = (*providers.GiteaProvider)(nil)
)

// remediationStageProvider builds the provider a pr-remediation stage talks
// to, dispatched by the routed repo's kind — the openpr.go per-kind idiom
// (github | gitea | default-error). ADO is default-error: *ADOProvider does
// not implement this broad surface (the CI/branch-tip reads among others), so
// an ADO-capable stage builds a narrow surface through remediationStageSurface
// instead, as pr-claim and the review-thread stages do.
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

// remediationStageSurface builds a pr-remediation stage's provider through a
// narrow surface T rather than the broad remediationProvider factory above,
// so a stage that only names the calls it actually makes routes through
// every registered provider — including ADO — rather than the GitHub/Gitea-
// only default-error dispatch. newProviderForStageSurface's own type
// assertion still fails loudly if a routed backend does not implement T, so
// this stays as safe as the broad factory for the surfaces GitHub and Gitea
// already satisfy. pr-claim (ADO-N14) uses it for its PR-poll-only surface;
// it is intended for reuse by other narrow pr-remediation surfaces.
func remediationStageSurface[T any](root string, repo providers.RepositoryRef, token string, opts ...stageProviderOption) (T, error) {
	allOpts := append([]stageProviderOption{withStageProviderToken(token)}, opts...)
	return newProviderForStageSurface[T](root, repo, false, allOpts...)
}

// reviewThreadReader is gather-review-threads' narrow provider surface: the
// one call it makes. GitHub, Gitea and ADO all implement it.
type reviewThreadReader interface {
	ListPullRequestReviewThreads(ctx context.Context, repo providers.RepositoryRef, pullID string) (providers.PullRequestReviewThreads, error)
}

// reviewThreadResolver is resolve-review-threads' narrow provider surface.
// Replying and resolving (providers.PullRequestReviewThreadMutator) stay a
// separate type assertion so a provider that can read but not mutate threads
// (Gitea) keeps its specific refusal.
type reviewThreadResolver interface {
	reviewThreadReader
	GetPullRequest(ctx context.Context, repo providers.RepositoryRef, pullID string) (providers.PullRequestSummary, error)
}

// reviewThreadStageSurface builds a review-thread stage's provider through
// remediationStageSurface, so ADO (ADO-N20) routes like GitHub and Gitea.
// Every provider is built from the stage's declared github:pr:write
// credential; on ADO it is sent in the daemon-delivered scheme, so every ADO
// auth kind backs it (ADO-N18) — the same rule as pr-claim. cached selects
// the GitHub-only conditional-GET read cache.
func reviewThreadStageSurface[T any](root string, repo providers.RepositoryRef, cached bool) (T, error) {
	var zero T
	token, err := providerToken(capability.GitHubPRWrite)
	if err != nil {
		return zero, err
	}
	opts := []stageProviderOption{withStageProviderCapability(capability.GitHubPRWrite)}
	if cached && repo.Provider == providers.ProviderGitHub {
		opts = append(opts, withStageProviderCache())
	}
	return remediationStageSurface[T](root, repo, token, opts...)
}
