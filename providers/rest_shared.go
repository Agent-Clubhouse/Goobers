package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	apiintegrity "github.com/goobers/goobers/api/integrity"
)

// The GitHub and Gitea REST surfaces are shape-compatible for the operations
// below, so each one lives here once, parameterized by the HTTP seam the
// calling provider satisfies (#4234). Keeping a per-provider copy meant a fix
// to one backend had no mechanism that reached the other, and the two drifted
// by omission.

// restSender is the raw request seam: one attempt plus the provider's own
// retry/rate-limit policy, with the response body still open.
type restSender interface {
	send(ctx context.Context, method, endpoint string, body interface{}) (*http.Response, error)
}

// restDoer issues a request and decodes a JSON response into out.
type restDoer interface {
	do(ctx context.Context, method, endpoint string, body, out interface{}) error
}

// restPager walks every page of a paginated collection (#139).
type restPager interface {
	getAllPages(ctx context.Context, endpoint string, onPage func([]byte) error) error
}

// restClaimReader is the seam the claim protocol needs: the full comment
// history plus the identity whose breadcrumbs are trusted.
type restClaimReader interface {
	restPager
	AuthenticatedLogin(ctx context.Context) (string, error)
}

// restComment is the issue-comment payload both backends return.
type restComment struct {
	ID        int64      `json:"id"`
	Body      string     `json:"body"`
	User      githubUser `json:"user"`
	HTMLURL   string     `json:"html_url"`
	CreatedAt *time.Time `json:"created_at"`
}

// restRepository is the repository payload both backends embed in a pull
// request's head/base branch.
type restRepository struct {
	Name    string     `json:"name"`
	HTMLURL string     `json:"html_url"`
	Owner   githubUser `json:"owner"`
}

// doStatus performs a request with the provider's transient-failure retries.
// Status codes in allowStatus are treated as success (used to tolerate a 404
// when removing a label that is not present); the response body is not decoded
// for those.
func doStatus(ctx context.Context, c restSender, method, endpoint string, body, out interface{}, allowStatus []int) error {
	resp, err := c.send(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	for _, code := range allowStatus {
		if resp.StatusCode == code {
			_ = resp.Body.Close()
			return nil
		}
	}
	return readJSONResponse(resp, method, endpoint, out)
}

// allIssueComments fetches every comment on an issue, following pagination
// (#139). Both ListComments and the claim protocol's claimWinner read the full
// comment set through here: a claim breadcrumb landing on page 2+ used to be
// invisible, so two racers each read "no claim" and both took the empty-read
// "we win" branch — a double claim on any issue with >30 comments.
func allIssueComments(ctx context.Context, c restPager, baseURL string, repo RepositoryRef, id string) ([]restComment, error) {
	endpoint, err := joinURL(baseURL, "repos", repo.Owner, repo.Name, "issues", id, "comments")
	if err != nil {
		return nil, err
	}
	var all []restComment
	err = c.getAllPages(ctx, endpoint, func(page []byte) error {
		var pageItems []restComment
		if err := json.Unmarshal(page, &pageItems); err != nil {
			return fmt.Errorf("decode comments page: %w", err)
		}
		all = append(all, pageItems...)
		return nil
	})
	return all, err
}

// claimWinner reads trusted issue comments and returns the run id of the recognized
// claimer in the current epoch. Only the authenticated provider identity can change
// epoch state; issue comments from other users are untrusted. A matching release
// breadcrumb ends an epoch, so stale winner and losing-racer breadcrumbs cannot
// block the next owner.
func claimWinner(ctx context.Context, c restClaimReader, baseURL string, repo RepositoryRef, id string) (string, bool, error) {
	markerAuthor, err := c.AuthenticatedLogin(ctx)
	if err != nil {
		return "", false, fmt.Errorf("resolve claim marker author: %w", err)
	}
	raw, err := allIssueComments(ctx, c, baseURL, repo, id)
	if err != nil {
		return "", false, err
	}
	sort.Slice(raw, func(i, j int) bool { return raw[i].ID < raw[j].ID })
	winner := ""
	for _, comment := range raw {
		if !strings.EqualFold(comment.User.Login, markerAuthor) {
			continue
		}
		if releasedBy := claimReleaseRunID(comment.Body); releasedBy != "" {
			if winner == releasedBy {
				winner = ""
			}
			continue
		}
		if winner == "" {
			winner = claimRunID(comment.Body)
		}
	}
	if winner == "" {
		return "", false, nil
	}
	return winner, true, nil
}

// postAttributedComment appends an issue comment carrying the run's
// attribution marker for the named action.
func postAttributedComment(ctx context.Context, c restDoer, baseURL string, attribution Attribution, repo RepositoryRef, id, body, action string) error {
	body, err := withAttribution(body, attribution, action)
	if err != nil {
		return err
	}
	endpoint, err := joinURL(baseURL, "repos", repo.Owner, repo.Name, "issues", id, "comments")
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, endpoint, map[string]string{"body": body}, nil)
}

// pullRequestComments lists a pull request's issue comments, optionally
// bounded to those updated since a watermark.
func pullRequestComments(ctx context.Context, c restPager, baseURL string, repo RepositoryRef, pullID string, since *time.Time) ([]PullRequestComment, error) {
	endpoint, err := joinURL(baseURL, "repos", repo.Owner, repo.Name, "issues", pullID, "comments")
	if err != nil {
		return nil, err
	}
	if since != nil {
		endpoint, err = addQuery(endpoint, url.Values{"since": []string{since.UTC().Format(time.RFC3339)}})
		if err != nil {
			return nil, err
		}
	}
	comments := make([]PullRequestComment, 0)
	if err := c.getAllPages(ctx, endpoint, func(page []byte) error {
		var raw []githubIssueComment
		if err := json.Unmarshal(page, &raw); err != nil {
			return fmt.Errorf("decode pull request comments page: %w", err)
		}
		for _, comment := range raw {
			comments = append(comments, PullRequestComment{
				ID: comment.ID, Author: comment.User.Login, Body: comment.Body, URL: comment.HTMLURL,
				CreatedAt: comment.CreatedAt, Integrity: apiintegrity.Unapproved,
			})
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return comments, nil
}

// normalizeCombinedStatusState maps a combined commit-status state onto the
// provider-neutral check state.
func normalizeCombinedStatusState(state string) CheckState {
	switch strings.ToLower(state) {
	case "success":
		return CheckStatePassing
	case "failure", "error":
		return CheckStateFailing
	default:
		return CheckStatePending
	}
}

// repositoryRef projects a REST repository payload onto a RepositoryRef for
// the given backend.
func repositoryRef(kind ProviderKind, repo *restRepository) *RepositoryRef {
	if repo == nil {
		return nil
	}
	return &RepositoryRef{
		Provider: kind,
		Owner:    repo.Owner.Login,
		Name:     repo.Name,
		URL:      repo.HTMLURL,
	}
}
