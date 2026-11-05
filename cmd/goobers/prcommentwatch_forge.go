package main

import (
	"context"
	"fmt"
	"strconv"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

// prCommentWatchForge is one provider's arm of the watcher: resolve the
// credential's identity, list the open PRs (labels ride the summary), read one
// PR's comments oldest-first, and route a PR to remediation while clearing the
// park labels it carries.
type prCommentWatchForge interface {
	identity(context.Context) (commentIdentity, error)
	listPullRequests(ctx context.Context, base string) ([]providers.PullRequestSummary, error)
	listComments(context.Context, providers.PullRequestSummary, commentClassifier) ([]providers.Comment, error)
	route(ctx context.Context, pr providers.PullRequestSummary, clear []string) error
	recordRouting(context.Context, providers.PullRequestSummary, string) error
	park(context.Context, providers.PullRequestSummary, []string) error
	// disarm cancels a landing armed on pr (goobers:merge-ready) before the
	// PR is routed for fresh human feedback (#6134). disarmed is true only
	// when this stage actually cancelled something.
	disarm(ctx context.Context, pr providers.PullRequestSummary) (disarmed bool, err error)
}

// newPRCommentWatchForge builds the provider arm through the shared
// stage-provider seam, never a backend constructor: the stage compares every
// comment author with the credential's identity, so a provider built off-seam
// is one with no declared identity — #3885/#3890's shape, and in a pod
// #3914's (under GitHub App auth AuthenticatedLogin fell back to GET /user,
// which an installation token cannot call).
//
// GitHub and Gitea route with an ordinary issues-API label write, so they take
// the github:issues:write credential. Azure DevOps routes with native
// pull-request labels on the project provider, which github:pr:write
// authorizes there (docs/design/ado-parity-dsl-2-0.md §3.1).
func newPRCommentWatchForge(root string, repo providers.RepositoryRef) (prCommentWatchForge, error) {
	switch repo.Provider {
	case providers.ProviderGitea, providers.ProviderGitHub:
		token, err := providerToken(capability.GitHubIssuesWrite)
		if err != nil {
			return nil, err
		}
		built, err := newProviderForStageSurface[prCommentWatchProvider](root, repo, false,
			withStageProviderCapability(capability.GitHubIssuesWrite),
			withStageProviderToken(token),
			withStageProviderMutations("pr"),
		)
		if err != nil {
			return nil, err
		}
		return issueLabelCommentWatch{provider: built, repo: repo}, nil
	case providers.ProviderADO:
		built, err := newProviderForStageSurface[adoPRCommentWatchProvider](root, repo, false,
			withStageProviderCapability(capability.GitHubPRWrite),
			withStageProviderMutations("pr"),
		)
		if err != nil {
			return nil, err
		}
		return adoCommentWatch{provider: built, repo: repo}, nil
	default:
		return nil, fmt.Errorf("pr-comment-watch does not support repository provider %q", repo.Provider)
	}
}

// prCommentWatchProvider is the GitHub and Gitea surface: the PR list, a PR's
// issue-comment thread, the token's own login, and the issues-API label write.
type prCommentWatchProvider interface {
	ListPullRequests(context.Context, providers.ListPullRequestsRequest) ([]providers.PullRequestSummary, error)
	ListComments(context.Context, providers.RepositoryRef, string) ([]providers.Comment, error)
	AuthenticatedLogin(context.Context) (string, error)
	UpdateWorkItem(context.Context, providers.UpdateWorkItemRequest) (providers.WorkItem, error)
}

type issueLabelCommentWatch struct {
	provider prCommentWatchProvider
	repo     providers.RepositoryRef
}

func (w issueLabelCommentWatch) identity(ctx context.Context) (commentIdentity, error) {
	login, err := w.provider.AuthenticatedLogin(ctx)
	if err != nil {
		return commentIdentity{}, err
	}
	return loginCommentIdentity(login), nil
}

func (w issueLabelCommentWatch) listPullRequests(ctx context.Context, base string) ([]providers.PullRequestSummary, error) {
	return w.provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository:     w.repo,
		Base:           base,
		SkipCheckState: true, // we never gate on CI; skipping it saves 2 API calls/PR
	})
}

func (w issueLabelCommentWatch) listComments(ctx context.Context, pr providers.PullRequestSummary, _ commentClassifier) ([]providers.Comment, error) {
	return w.provider.ListComments(ctx, w.repo, strconv.Itoa(pr.Number))
}

// route adds needs-remediation and strips the carried park labels in one
// issues-API mutation, addressing the PR by its number as an issue id
// (applyverdict.go precedent). Re-adding an existing label is a no-op.
func (w issueLabelCommentWatch) route(ctx context.Context, pr providers.PullRequestSummary, clear []string) error {
	_, err := w.provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository:   w.repo,
		ID:           strconv.Itoa(pr.Number),
		AddLabels:    []string{needsRemediationLabel},
		RemoveLabels: clear,
	})
	return err
}

// disarm is a no-op here: GitHub's merge queue is the only landing Goobers
// arms, and dequeuing takes the merge authority this stage does not hold.
// queue-watch, which holds it while the PR is queued, dequeues a PR that
// gains goobers:needs-remediation or an explicit human-hold label.
func (issueLabelCommentWatch) disarm(context.Context, providers.PullRequestSummary) (bool, error) {
	return false, nil
}

// adoPRCommentWatchProvider is the Azure DevOps surface: the PR list (labels
// via includeLabels), every live comment across general and file threads, the
// credential's identity by immutable id, and the native PR-label endpoints —
// never UpdateWorkItem(ID: PR#), which would address the unrelated work item
// sharing the PR's numeric id.
type adoPRCommentWatchProvider interface {
	PostPullRequestThreadComment(context.Context, providers.RepositoryRef, string, string) (providers.Comment, error)
	ListPullRequests(context.Context, providers.ListPullRequestsRequest) ([]providers.PullRequestSummary, error)
	ListPullRequestFeedbackComments(ctx context.Context, repo providers.RepositoryRef, pullID string) ([]providers.Comment, error)
	AuthenticatedIdentity(context.Context) (providers.ADOIdentity, error)
	AddPullRequestLabels(ctx context.Context, repo providers.RepositoryRef, pullID string, names []string) error
	RemovePullRequestLabel(ctx context.Context, repo providers.RepositoryRef, pullID, name string) error
	CancelAutoComplete(ctx context.Context, repo providers.RepositoryRef, pullID string) (providers.AutoCompleteCancel, error)
	ListPullRequestFeedbackThreads(ctx context.Context, repo providers.RepositoryRef, pullID string) ([]providers.PullRequestFeedbackThread, error)
}

type adoCommentWatch struct {
	provider adoPRCommentWatchProvider
	repo     providers.RepositoryRef
}

func (w adoCommentWatch) identity(ctx context.Context) (commentIdentity, error) {
	self, err := w.provider.AuthenticatedIdentity(ctx)
	if err != nil {
		return commentIdentity{}, err
	}
	return adoCommentIdentity(self), nil
}

func (w adoCommentWatch) listPullRequests(ctx context.Context, base string) ([]providers.PullRequestSummary, error) {
	return w.provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{Repository: w.repo, Base: base, SkipCheckState: true})
}

// listComments reads every live thread comment. For a PR that is landing
// (goobers:merge-ready) it reads the OPEN feedback only, as merge-pr's gate
// does: a reviewer who resolved their own thread must not cancel an armed
// auto-complete over feedback the gate would not refuse on.
func (w adoCommentWatch) listComments(ctx context.Context, pr providers.PullRequestSummary, classifier commentClassifier) ([]providers.Comment, error) {
	pullID := strconv.Itoa(pr.Number)
	if !hasAnyLabel(pr.Labels, []string{mergeReadyLabel}) {
		return w.provider.ListPullRequestFeedbackComments(ctx, w.repo, pullID)
	}
	threads, err := w.provider.ListPullRequestFeedbackThreads(ctx, w.repo, pullID)
	if err != nil {
		return nil, err
	}
	return openFeedbackComments(nil, adoFeedbackThreads(threads), classifier), nil
}

// disarm cancels the pull request's armed auto-complete: ADO would otherwise
// complete it the moment its pending policies settle, over the feedback.
func (w adoCommentWatch) disarm(ctx context.Context, pr providers.PullRequestSummary) (bool, error) {
	outcome, err := w.provider.CancelAutoComplete(ctx, w.repo, strconv.Itoa(pr.Number))
	return outcome == providers.AutoCompleteCancelled, err
}

// route clears the carried park labels, then adds needs-remediation. ADO has
// no single label mutation, so the order is chosen for retry convergence: a
// failure after a clear leaves an un-parked, unrouted PR whose human comment
// is still the newest, which the next tick routes; the reverse order could
// leave a routed PR still parked, which the needs-remediation exclusion then
// hides from every later tick.
func (w adoCommentWatch) route(ctx context.Context, pr providers.PullRequestSummary, clear []string) error {
	pullID := strconv.Itoa(pr.Number)
	for _, label := range clear {
		if err := w.provider.RemovePullRequestLabel(ctx, w.repo, pullID, label); err != nil {
			return fmt.Errorf("clear %s: %w", label, err)
		}
	}
	return w.provider.AddPullRequestLabels(ctx, w.repo, pullID, []string{needsRemediationLabel})
}

func (w issueLabelCommentWatch) recordRouting(ctx context.Context, pr providers.PullRequestSummary, body string) error {
	_, err := w.provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{Repository: w.repo, ID: strconv.Itoa(pr.Number), Comment: body})
	return err
}
func (w adoCommentWatch) recordRouting(ctx context.Context, pr providers.PullRequestSummary, body string) error {
	_, err := w.provider.PostPullRequestThreadComment(ctx, w.repo, strconv.Itoa(pr.Number), body)
	return err
}
func (w issueLabelCommentWatch) park(ctx context.Context, pr providers.PullRequestSummary, clear []string) error {
	_, err := w.provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository: w.repo, ID: strconv.Itoa(pr.Number), AddLabels: []string{remediationEscalatedLabel, providers.LabelNeedsHuman}, RemoveLabels: clear,
	})
	return err
}
func (w adoCommentWatch) park(ctx context.Context, pr providers.PullRequestSummary, clear []string) error {
	// Establish the human hold before removing labels, so a partial failure
	// cannot leave the PR eligible for automatic landing or remediation.
	if err := w.provider.AddPullRequestLabels(ctx, w.repo, strconv.Itoa(pr.Number), []string{remediationEscalatedLabel, providers.LabelNeedsHuman}); err != nil {
		return err
	}
	for _, label := range clear {
		if err := w.provider.RemovePullRequestLabel(ctx, w.repo, strconv.Itoa(pr.Number), label); err != nil {
			return err
		}
	}
	return nil
}
