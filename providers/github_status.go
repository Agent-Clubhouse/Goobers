package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// githubStatusDescriptionLimit is the longest description GitHub's commit
// status API accepts; a longer one is refused with 422, which would fail the
// whole publication over prose.
const githubStatusDescriptionLimit = 140

// PublishPullRequestStatus posts a GitHub commit status (POST
// /repos/{owner}/{repo}/statuses/{sha}) that a required status check in
// branch protection or a ruleset can gate on (#4199). A commit status belongs
// to one commit, so evidence published for the pinned req.HeadSHA never
// reaches a newer head; with no HeadSHA the pull request's live head is used.
// The context is genre/name, as on Gitea.
func (p *GitHubProvider) PublishPullRequestStatus(ctx context.Context, req PullRequestStatusRequest) (PullRequestStatusResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return PullRequestStatusResult{}, err
	}
	if strings.TrimSpace(req.PullID) == "" {
		return PullRequestStatusResult{}, errPullIDRequired
	}
	if req.Name == "" {
		return PullRequestStatusResult{}, fmt.Errorf("status name is required")
	}
	headSHA := strings.TrimSpace(req.HeadSHA)
	if headSHA == "" {
		current, err := p.GetPullRequest(ctx, req.Repository, req.PullID)
		if err != nil {
			return PullRequestStatusResult{}, fmt.Errorf("resolve pull request head: %w", err)
		}
		if current.HeadSHA == "" {
			return PullRequestStatusResult{}, fmt.Errorf("pull request %s has no head sha", req.PullID)
		}
		headSHA = current.HeadSHA
	}
	genre := req.Genre
	if genre == "" {
		genre = MergeReviewStatusGenre
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "statuses", headSHA)
	if err != nil {
		return PullRequestStatusResult{}, err
	}
	body := map[string]interface{}{
		"context": genre + "/" + req.Name,
		"state":   githubStatusState(req.State),
	}
	if description := truncateRunes(req.Description, githubStatusDescriptionLimit); description != "" {
		body["description"] = description
	}
	if req.TargetURL != "" {
		body["target_url"] = req.TargetURL
	}
	var out struct {
		ID int `json:"id"`
	}
	if err := p.do(ctx, http.MethodPost, endpoint, body, &out); err != nil {
		return PullRequestStatusResult{}, err
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider:  ProviderGitHub,
		Ref:       fmt.Sprintf("%s/%s@%s", req.Repository.Owner, req.Repository.Name, headSHA),
		URL:       req.TargetURL,
		Operation: "status",
	})
	return PullRequestStatusResult{ID: out.ID}, nil
}

// MergeReviewStatus reads the merge-review verdict status standing on ref
// (the latest one GitHub's combined status reports for that context), or nil
// when there is none (#4199).
func (p *GitHubProvider) MergeReviewStatus(ctx context.Context, repo RepositoryRef, ref string) (*CheckDetail, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return nil, err
	}
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "commits", ref, "status")
	if err != nil {
		return nil, err
	}
	var verdict *CheckDetail
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var combined githubCombinedStatus
		if err := json.Unmarshal(page, &combined); err != nil {
			return fmt.Errorf("decode combined status page: %w", err)
		}
		for _, status := range combined.Statuses {
			if found, ok := mergeReviewStatusDetail(status.Context, status.State, status.Description, status.TargetURL); ok {
				verdict = found
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return verdict, nil
}

func githubStatusState(state CheckState) string {
	switch state {
	case CheckStatePassing:
		return "success"
	case CheckStateFailing:
		return "failure"
	default:
		return "pending"
	}
}

// IsStatusLimitError reports whether GitHub refused a commit status because
// the commit already carries the most statuses one context may have (1000).
// Re-publishing an unchanged verdict is skipped, so reaching the cap means a
// head re-reviewed with a changing verdict a thousand times; the standing
// status is still the latest one the provider accepted.
func IsStatusLimitError(err error) bool {
	var responseErr *providerResponseError
	return errors.As(err, &responseErr) && responseErr.statusCode == http.StatusUnprocessableEntity &&
		strings.Contains(strings.ToLower(responseErr.body), "maximum number of statuses")
}

// IsStatusPublishDeniedError reports whether a provider refused to publish a
// commit or pull-request status because the credential may not write
// statuses: a 403 that carries no rate-limit guidance, or the 404 GitHub
// answers a token that cannot see the status surface. It lets a caller tell
// a missing grant (the operator must add Commit statuses: Read and write)
// from a transient failure worth retrying.
func IsStatusPublishDeniedError(err error) bool {
	var responseErr *providerResponseError
	if !errors.As(err, &responseErr) {
		return false
	}
	switch responseErr.statusCode {
	case http.StatusForbidden:
		return !responseErr.hasRetryGuidance()
	case http.StatusNotFound:
		return true
	default:
		return false
	}
}
