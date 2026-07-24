package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

const pullRequestReviewThreadsQuery = `query($owner: String!, $name: String!, $number: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $after) {
        nodes {
          isResolved
          isOutdated
          path
          line
          originalLine
          diffSide
          startLine
          originalStartLine
          startDiffSide
          comments(first: 100) {
            nodes {
              databaseId
            }
          }
        }
        pageInfo {
          hasNextPage
          endCursor
        }
      }
    }
  }
}`

type githubNativeReview struct {
	ID          int64      `json:"id"`
	Body        string     `json:"body"`
	State       string     `json:"state"`
	HTMLURL     string     `json:"html_url"`
	CommitID    string     `json:"commit_id"`
	SubmittedAt *time.Time `json:"submitted_at"`
	User        githubUser `json:"user"`
}

type githubInlineReviewComment struct {
	ID                int64      `json:"id"`
	Body              string     `json:"body"`
	Path              string     `json:"path"`
	Line              *int       `json:"line"`
	OriginalLine      *int       `json:"original_line"`
	Side              string     `json:"side"`
	StartLine         *int       `json:"start_line"`
	OriginalStartLine *int       `json:"original_start_line"`
	StartSide         string     `json:"start_side"`
	DiffHunk          string     `json:"diff_hunk"`
	InReplyTo         int64      `json:"in_reply_to_id"`
	CreatedAt         *time.Time `json:"created_at"`
	HTMLURL           string     `json:"html_url"`
	User              githubUser `json:"user"`
}

type githubReviewThreadState struct {
	IsResolved        bool
	IsOutdated        bool
	Path              string
	Line              int
	OriginalLine      int
	Side              string
	StartLine         int
	OriginalStartLine int
	StartSide         string
}

type githubReviewThreadsPage struct {
	Repository struct {
		PullRequest *struct {
			ReviewThreads struct {
				Nodes []struct {
					IsResolved        bool   `json:"isResolved"`
					IsOutdated        bool   `json:"isOutdated"`
					Path              string `json:"path"`
					Line              *int   `json:"line"`
					OriginalLine      *int   `json:"originalLine"`
					DiffSide          string `json:"diffSide"`
					StartLine         *int   `json:"startLine"`
					OriginalStartLine *int   `json:"originalStartLine"`
					StartDiffSide     string `json:"startDiffSide"`
					Comments          struct {
						Nodes []struct {
							DatabaseID int64 `json:"databaseId"`
						} `json:"nodes"`
					} `json:"comments"`
				} `json:"nodes"`
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"reviewThreads"`
		} `json:"pullRequest"`
	} `json:"repository"`
}

// ListPullRequestReviewThreads returns native review bodies and every inline
// comment. REST supplies complete paginated bodies and anchors; GraphQL supplies
// resolved/outdated thread state, which GitHub does not expose through REST.
func (p *GitHubProvider) ListPullRequestReviewThreads(ctx context.Context, repo RepositoryRef, pullID string) (PullRequestReviewThreads, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return PullRequestReviewThreads{}, err
	}
	number, err := strconv.Atoi(pullID)
	if err != nil || number < 1 {
		return PullRequestReviewThreads{}, fmt.Errorf("pull id must be a positive integer")
	}

	reviews, err := p.listNativePullRequestReviews(ctx, repo, pullID)
	if err != nil {
		return PullRequestReviewThreads{}, err
	}
	threadStates, err := p.pullRequestReviewThreadStates(ctx, repo, number)
	if err != nil {
		return PullRequestReviewThreads{}, err
	}
	comments, err := p.listInlinePullRequestComments(ctx, repo, pullID, threadStates)
	if err != nil {
		return PullRequestReviewThreads{}, err
	}
	return PullRequestReviewThreads{Reviews: reviews, InlineComments: comments}, nil
}

func (p *GitHubProvider) listNativePullRequestReviews(ctx context.Context, repo RepositoryRef, pullID string) ([]PullRequestNativeReview, error) {
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "pulls", pullID, "reviews")
	if err != nil {
		return nil, err
	}
	reviews := make([]PullRequestNativeReview, 0)
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var raw []githubNativeReview
		if err := json.Unmarshal(page, &raw); err != nil {
			return fmt.Errorf("decode pull request reviews page: %w", err)
		}
		for _, review := range raw {
			reviews = append(reviews, PullRequestNativeReview{
				ID:          review.ID,
				Author:      review.User.Login,
				State:       review.State,
				Body:        review.Body,
				CommitSHA:   review.CommitID,
				SubmittedAt: review.SubmittedAt,
				URL:         review.HTMLURL,
			})
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return reviews, nil
}

func (p *GitHubProvider) listInlinePullRequestComments(ctx context.Context, repo RepositoryRef, pullID string, states map[int64]githubReviewThreadState) ([]PullRequestInlineComment, error) {
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "pulls", pullID, "comments")
	if err != nil {
		return nil, err
	}
	comments := make([]PullRequestInlineComment, 0)
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var raw []githubInlineReviewComment
		if err := json.Unmarshal(page, &raw); err != nil {
			return fmt.Errorf("decode inline review comments page: %w", err)
		}
		for _, comment := range raw {
			state, ok := states[comment.ID]
			if !ok && comment.InReplyTo != 0 {
				state, ok = states[comment.InReplyTo]
			}
			if !ok {
				return fmt.Errorf("inline review comment %d has no review-thread state", comment.ID)
			}
			path := comment.Path
			if path == "" {
				path = state.Path
			}
			line := state.Line
			if comment.Line != nil {
				line = *comment.Line
			}
			originalLine := state.OriginalLine
			if comment.OriginalLine != nil {
				originalLine = *comment.OriginalLine
			}
			side := comment.Side
			if side == "" {
				side = state.Side
			}
			startLine := state.StartLine
			if comment.StartLine != nil {
				startLine = *comment.StartLine
			}
			originalStartLine := state.OriginalStartLine
			if comment.OriginalStartLine != nil {
				originalStartLine = *comment.OriginalStartLine
			}
			startSide := comment.StartSide
			if startSide == "" {
				startSide = state.StartSide
			}
			comments = append(comments, PullRequestInlineComment{
				ID:                comment.ID,
				Author:            comment.User.Login,
				Body:              comment.Body,
				Path:              path,
				Line:              line,
				OriginalLine:      originalLine,
				Side:              side,
				StartLine:         startLine,
				OriginalStartLine: originalStartLine,
				StartSide:         startSide,
				DiffHunk:          comment.DiffHunk,
				InReplyTo:         comment.InReplyTo,
				IsResolved:        state.IsResolved,
				IsOutdated:        state.IsOutdated,
				CreatedAt:         comment.CreatedAt,
				URL:               comment.HTMLURL,
			})
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return comments, nil
}

func (p *GitHubProvider) pullRequestReviewThreadStates(ctx context.Context, repo RepositoryRef, number int) (map[int64]githubReviewThreadState, error) {
	states := make(map[int64]githubReviewThreadState)
	var after interface{}
	for {
		var page githubReviewThreadsPage
		if err := p.graphql(ctx, pullRequestReviewThreadsQuery, map[string]interface{}{
			"owner": repo.Owner, "name": repo.Name, "number": number, "after": after,
		}, &page); err != nil {
			return nil, err
		}
		if page.Repository.PullRequest == nil {
			return nil, fmt.Errorf("pull request %s/%s#%d not found", repo.Owner, repo.Name, number)
		}
		threads := page.Repository.PullRequest.ReviewThreads
		for _, thread := range threads.Nodes {
			state := githubReviewThreadState{
				IsResolved: thread.IsResolved,
				IsOutdated: thread.IsOutdated,
				Path:       thread.Path,
				Side:       thread.DiffSide,
				StartSide:  thread.StartDiffSide,
			}
			if thread.Line != nil {
				state.Line = *thread.Line
			}
			if thread.OriginalLine != nil {
				state.OriginalLine = *thread.OriginalLine
			}
			if thread.StartLine != nil {
				state.StartLine = *thread.StartLine
			}
			if thread.OriginalStartLine != nil {
				state.OriginalStartLine = *thread.OriginalStartLine
			}
			for _, comment := range thread.Comments.Nodes {
				if comment.DatabaseID != 0 {
					states[comment.DatabaseID] = state
				}
			}
		}
		if !threads.PageInfo.HasNextPage {
			return states, nil
		}
		if threads.PageInfo.EndCursor == "" {
			return nil, fmt.Errorf("github review-thread pagination returned an empty cursor")
		}
		after = threads.PageInfo.EndCursor
	}
}

var _ PullRequestReviewThreadProvider = (*GitHubProvider)(nil)
