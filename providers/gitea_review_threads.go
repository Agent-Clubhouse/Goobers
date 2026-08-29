package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	apiintegrity "github.com/goobers/goobers/api/integrity"
)

// giteaPullReview is one native Gitea pull-request review. Distinct from
// giteaReview (gitea.go), which carries only State+User for reviewDecision and
// must stay narrow — reviewDecision's aggregation depends on that narrowness.
type giteaPullReview struct {
	ID          int64      `json:"id"`
	User        githubUser `json:"user"`
	State       string     `json:"state"`
	Body        string     `json:"body"`
	CommitID    string     `json:"commit_id"`
	SubmittedAt *time.Time `json:"submitted_at"`
	HTMLURL     string     `json:"html_url"`
}

// giteaPullReviewComment is one inline comment attached to a Gitea review.
// Gitea anchors inline comments by diff position, not file line, and exposes no
// thread resolution/outdatedness state.
type giteaPullReviewComment struct {
	ID               int64      `json:"id"`
	Body             string     `json:"body"`
	Path             string     `json:"path"`
	Position         int64      `json:"position"`
	OriginalPosition int64      `json:"original_position"`
	DiffHunk         string     `json:"diff_hunk"`
	CreatedAt        *time.Time `json:"created_at"`
	HTMLURL          string     `json:"html_url"`
	User             githubUser `json:"user"`
}

// ListPullRequestReviewThreads returns native review bodies and every inline
// review comment for a Gitea pull request. Gitea has no GraphQL and no flat
// "all inline comments" endpoint, so this uses REST only: the pull's reviews,
// then each review's inline comments.
//
// Documented degradations relative to the GitHub twin:
//   - IsResolved/IsOutdated are always false — Gitea's REST review-comment
//     payload does not expose thread resolution or outdatedness. Consequence:
//     gather-review-threads presents every inline comment as live, so the
//     remediator sees strictly MORE feedback, never less (fail-open in the safe
//     direction).
//   - Line/OriginalLine are mapped best-effort from Gitea's diff
//     position/original_position (diff positions, not file lines); Side,
//     StartLine, OriginalStartLine, StartSide, and InReplyTo have no Gitea
//     equivalent and stay zero. There is no thread-state join, so unlike the
//     GitHub twin every comment maps unconditionally (no "comment has no
//     review-thread state" error path).
func (p *GiteaProvider) ListPullRequestReviewThreads(ctx context.Context, repo RepositoryRef, pullID string) (PullRequestReviewThreads, error) {
	if err := p.ready(); err != nil {
		return PullRequestReviewThreads{}, err
	}
	if err := requireOwnerRepo(repo); err != nil {
		return PullRequestReviewThreads{}, err
	}
	number, err := strconv.Atoi(pullID)
	if err != nil || number < 1 {
		return PullRequestReviewThreads{}, fmt.Errorf("pull id must be a positive integer")
	}

	rawReviews, err := p.listGiteaPullReviews(ctx, repo, pullID)
	if err != nil {
		return PullRequestReviewThreads{}, err
	}
	reviews := make([]PullRequestNativeReview, 0, len(rawReviews))
	comments := make([]PullRequestInlineComment, 0)
	for _, review := range rawReviews {
		reviews = append(reviews, PullRequestNativeReview{
			ID:          review.ID,
			Author:      review.User.Login,
			State:       review.State,
			Body:        review.Body,
			CommitSHA:   review.CommitID,
			SubmittedAt: review.SubmittedAt,
			URL:         review.HTMLURL,
			Integrity:   apiintegrity.Unapproved,
		})
		rawComments, err := p.listGiteaPullReviewComments(ctx, repo, pullID, review.ID)
		if err != nil {
			return PullRequestReviewThreads{}, err
		}
		for _, comment := range rawComments {
			comments = append(comments, PullRequestInlineComment{
				ID:           comment.ID,
				ThreadID:     fmt.Sprintf("gitea-review-%d", review.ID),
				Author:       comment.User.Login,
				Body:         comment.Body,
				Path:         comment.Path,
				Line:         int(comment.Position),
				OriginalLine: int(comment.OriginalPosition),
				DiffHunk:     comment.DiffHunk,
				IsResolved:   false,
				IsOutdated:   false,
				CreatedAt:    comment.CreatedAt,
				URL:          comment.HTMLURL,
				Integrity:    apiintegrity.Unapproved,
			})
		}
	}
	return PullRequestReviewThreads{
		Reviews: reviews, InlineComments: comments, Integrity: apiintegrity.Unapproved,
	}, nil
}

// listGiteaPullReviews returns every native review on a pull request, following
// page/limit pagination.
func (p *GiteaProvider) listGiteaPullReviews(ctx context.Context, repo RepositoryRef, pullID string) ([]giteaPullReview, error) {
	var all []giteaPullReview
	const limit = 50
	for page := 1; ; page++ {
		endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "pulls", pullID, "reviews")
		if err != nil {
			return nil, err
		}
		endpoint, err = addQuery(endpoint, url.Values{
			"page":  []string{strconv.Itoa(page)},
			"limit": []string{strconv.Itoa(limit)},
		})
		if err != nil {
			return nil, err
		}
		var pageOut []giteaPullReview
		if err := p.do(ctx, http.MethodGet, endpoint, nil, &pageOut); err != nil {
			return nil, err
		}
		all = append(all, pageOut...)
		if len(pageOut) < limit {
			break
		}
	}
	return all, nil
}

// listGiteaPullReviewComments returns every inline comment on one review,
// following page/limit pagination.
func (p *GiteaProvider) listGiteaPullReviewComments(ctx context.Context, repo RepositoryRef, pullID string, reviewID int64) ([]giteaPullReviewComment, error) {
	var all []giteaPullReviewComment
	const limit = 50
	for page := 1; ; page++ {
		endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "pulls", pullID, "reviews", strconv.FormatInt(reviewID, 10), "comments")
		if err != nil {
			return nil, err
		}
		endpoint, err = addQuery(endpoint, url.Values{
			"page":  []string{strconv.Itoa(page)},
			"limit": []string{strconv.Itoa(limit)},
		})
		if err != nil {
			return nil, err
		}
		var pageOut []giteaPullReviewComment
		if err := p.do(ctx, http.MethodGet, endpoint, nil, &pageOut); err != nil {
			return nil, err
		}
		all = append(all, pageOut...)
		if len(pageOut) < limit {
			break
		}
	}
	return all, nil
}

var _ PullRequestReviewThreadProvider = (*GiteaProvider)(nil)
