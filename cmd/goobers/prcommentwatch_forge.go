package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

// Identity modes for pr-comment-watch. dedicated: Goobers runs as its own
// bot identity, so an unmarked comment by that identity is still Goobers'.
// shared: Goobers runs as a person's own identity (on Azure DevOps, typically
// the operator's own az login), so only a Goobers marker tells its comments
// apart and an unmarked comment by that identity is the person's.
const (
	prCommentWatchIdentityDedicated = "dedicated"
	prCommentWatchIdentityShared    = "shared"
)

// defaultPRCommentWatchIdentityMode is shared on Azure DevOps, whose
// recommended local authentication is the operator's own identity, and
// dedicated elsewhere, where a bot account has always been required.
func defaultPRCommentWatchIdentityMode(kind providers.ProviderKind) string {
	if kind == providers.ProviderADO {
		return prCommentWatchIdentityShared
	}
	return prCommentWatchIdentityDedicated
}

// prCommentWatchIdentity is the identity the stage's credential authenticates
// as. key is what comment authors are compared against: the login on GitHub
// and Gitea, the immutable identity id on Azure DevOps (byID), where display
// names are neither stable nor unique.
type prCommentWatchIdentity struct {
	login string
	key   string
	byID  bool
}

// prCommentWatchForge is one provider's arm of the watcher: resolve the
// credential's identity, list the open PRs (labels ride the summary), read one
// PR's comments oldest-first, and route a PR to remediation while clearing the
// park labels it carries.
type prCommentWatchForge interface {
	identity(context.Context) (prCommentWatchIdentity, error)
	listPullRequests(ctx context.Context, base string) ([]providers.PullRequestSummary, error)
	listComments(context.Context, providers.PullRequestSummary) ([]providers.Comment, error)
	route(ctx context.Context, pr providers.PullRequestSummary, clear []string) error
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

func (w issueLabelCommentWatch) identity(ctx context.Context) (prCommentWatchIdentity, error) {
	login, err := w.provider.AuthenticatedLogin(ctx)
	if err != nil {
		return prCommentWatchIdentity{}, err
	}
	return prCommentWatchIdentity{login: login, key: login}, nil
}

func (w issueLabelCommentWatch) listPullRequests(ctx context.Context, base string) ([]providers.PullRequestSummary, error) {
	return w.provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository:     w.repo,
		Base:           base,
		SkipCheckState: true, // we never gate on CI; skipping it saves 2 API calls/PR
	})
}

func (w issueLabelCommentWatch) listComments(ctx context.Context, pr providers.PullRequestSummary) ([]providers.Comment, error) {
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

// adoPRCommentWatchProvider is the Azure DevOps surface: the PR list (labels
// via includeLabels), every live comment across general and file threads, the
// credential's identity by immutable id, and the native PR-label endpoints —
// never UpdateWorkItem(ID: PR#), which would address the unrelated work item
// sharing the PR's numeric id.
type adoPRCommentWatchProvider interface {
	ListPullRequests(context.Context, providers.ListPullRequestsRequest) ([]providers.PullRequestSummary, error)
	ListPullRequestFeedbackComments(ctx context.Context, repo providers.RepositoryRef, pullID string) ([]providers.Comment, error)
	AuthenticatedIdentity(context.Context) (providers.ADOIdentity, error)
	AddPullRequestLabels(ctx context.Context, repo providers.RepositoryRef, pullID string, names []string) error
	RemovePullRequestLabel(ctx context.Context, repo providers.RepositoryRef, pullID, name string) error
}

type adoCommentWatch struct {
	provider adoPRCommentWatchProvider
	repo     providers.RepositoryRef
}

func (w adoCommentWatch) identity(ctx context.Context) (prCommentWatchIdentity, error) {
	self, err := w.provider.AuthenticatedIdentity(ctx)
	if err != nil {
		return prCommentWatchIdentity{}, err
	}
	login := self.UniqueName
	if login == "" {
		login = self.DisplayName
	}
	return prCommentWatchIdentity{login: login, key: self.ID, byID: true}, nil
}

func (w adoCommentWatch) listPullRequests(ctx context.Context, base string) ([]providers.PullRequestSummary, error) {
	return w.provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{Repository: w.repo, Base: base, SkipCheckState: true})
}

func (w adoCommentWatch) listComments(ctx context.Context, pr providers.PullRequestSummary) ([]providers.Comment, error) {
	return w.provider.ListPullRequestFeedbackComments(ctx, w.repo, strconv.Itoa(pr.Number))
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

// prCommentOrigin is who a comment speaks for in the watermark.
type prCommentOrigin int

const (
	prCommentFromHuman prCommentOrigin = iota
	prCommentFromGoobers
	prCommentFromAutomation
)

// prCommentClassifier decides each comment's origin. The rule, in order:
//
//  1. A comment carrying a Goobers marker on a line of its own, outside a
//     quote, is Goobers' own, whoever the author is.
//  2. In dedicated mode, an unmarked comment whose author is the credential's
//     own identity is Goobers' own too.
//  3. A Bot-typed author (GitHub) or one named in excludeAuthors is third-party
//     automation and takes part in neither watermark.
//  4. Everything else is human — including, in shared mode, an unmarked
//     comment by the credential's own identity.
type prCommentClassifier struct {
	selfKey        string
	byID           bool
	dedicated      bool
	excludeAuthors map[string]bool
}

func newPRCommentClassifier(self prCommentWatchIdentity, settings prCommentWatchSettings) prCommentClassifier {
	return prCommentClassifier{
		selfKey:        self.key,
		byID:           self.byID,
		dedicated:      settings.identityMode == prCommentWatchIdentityDedicated,
		excludeAuthors: settings.excludeAuthors,
	}
}

func (c prCommentClassifier) origin(comment providers.Comment) prCommentOrigin {
	if goobersMarkedComment(comment.Body) {
		return prCommentFromGoobers
	}
	author := comment.Author
	if c.byID {
		author = comment.AuthorID
	}
	if c.dedicated && author != "" && strings.EqualFold(author, c.selfKey) {
		return prCommentFromGoobers
	}
	if strings.EqualFold(comment.AuthorType, "bot") || c.excludeAuthors[strings.ToLower(author)] {
		return prCommentFromAutomation
	}
	return prCommentFromHuman
}

// goobersMarkedComment reports whether body carries a marker only Goobers
// writes: a valid attribution footer (providers.ParseAttribution) or a
// review-thread response marker, each on a line of its own. Quoted lines are
// ignored first, so a person quoting a Goobers comment in a reply is still a
// person.
func goobersMarkedComment(body string) bool {
	lines := strings.Split(body, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, ">") {
			continue
		}
		if strings.HasPrefix(trimmed, reviewThreadResponseMarkerPrefix) {
			return true
		}
		kept = append(kept, line)
	}
	unquoted := strings.Join(kept, "\n")
	if !strings.Contains(unquoted, "\n"+providers.AttributionMarkerPrefix) && !strings.HasPrefix(unquoted, providers.AttributionMarkerPrefix) {
		return false
	}
	_, ok, err := providers.ParseAttribution(unquoted)
	return ok && err == nil
}
