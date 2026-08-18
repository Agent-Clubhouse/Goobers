package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	apiintegrity "github.com/goobers/goobers/api/integrity"
)

const pullRequestReviewThreadsQuery = `query($owner: String!, $name: String!, $number: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $after) {
        nodes {
          id
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

const resolveReviewThreadMutation = `mutation($threadId: ID!) {
  resolveReviewThread(input: {threadId: $threadId}) {
    thread { id isResolved }
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
	ThreadID          string
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
					ID                string `json:"id"`
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
	// Snapshot comments first so comments created between the REST and GraphQL
	// reads are present in the later thread-state snapshot.
	rawComments, err := p.listInlinePullRequestComments(ctx, repo, pullID)
	if err != nil {
		return PullRequestReviewThreads{}, err
	}
	threadStates, err := p.pullRequestReviewThreadStates(ctx, repo, number)
	if err != nil {
		return PullRequestReviewThreads{}, err
	}
	comments, err := pullRequestInlineComments(rawComments, threadStates)
	if err != nil {
		return PullRequestReviewThreads{}, err
	}
	return PullRequestReviewThreads{
		Reviews: reviews, InlineComments: comments, Integrity: apiintegrity.Unapproved,
	}, nil
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
				Integrity:   apiintegrity.Unapproved,
			})
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return reviews, nil
}

func (p *GitHubProvider) listInlinePullRequestComments(ctx context.Context, repo RepositoryRef, pullID string) ([]githubInlineReviewComment, error) {
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "pulls", pullID, "comments")
	if err != nil {
		return nil, err
	}
	comments := make([]githubInlineReviewComment, 0)
	if err := p.getAllPages(ctx, endpoint, func(page []byte) error {
		var pageComments []githubInlineReviewComment
		if err := json.Unmarshal(page, &pageComments); err != nil {
			return fmt.Errorf("decode inline review comments page: %w", err)
		}
		comments = append(comments, pageComments...)
		return nil
	}); err != nil {
		return nil, err
	}
	return comments, nil
}

func pullRequestInlineComments(rawComments []githubInlineReviewComment, states map[int64]githubReviewThreadState) ([]PullRequestInlineComment, error) {
	comments := make([]PullRequestInlineComment, 0, len(rawComments))
	for _, comment := range rawComments {
		state, ok := states[comment.ID]
		if !ok && comment.InReplyTo != 0 {
			state, ok = states[comment.InReplyTo]
		}
		if !ok {
			return nil, fmt.Errorf("inline review comment %d has no review-thread state", comment.ID)
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
			ThreadID:          state.ThreadID,
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
			Integrity:         apiintegrity.Unapproved,
		})
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
				ThreadID:   thread.ID,
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

// ReplyPullRequestReviewThread posts one reply to an existing review thread.
func (p *GitHubProvider) ReplyPullRequestReviewThread(ctx context.Context, req PullRequestReviewThreadReply) (PullRequestInlineComment, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return PullRequestInlineComment{}, err
	}
	number, err := strconv.Atoi(req.PullID)
	if err != nil || number < 1 || req.CommentID < 1 || strings.TrimSpace(req.Body) == "" {
		return PullRequestInlineComment{}, fmt.Errorf("pull id, comment id, and reply body are required")
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls", req.PullID, "comments", strconv.FormatInt(req.CommentID, 10), "replies")
	if err != nil {
		return PullRequestInlineComment{}, err
	}
	var created githubInlineReviewComment
	if err := p.do(ctx, http.MethodPost, endpoint, map[string]string{"body": req.Body}, &created); err != nil {
		return PullRequestInlineComment{}, err
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider: ProviderGitHub, Ref: issueRef(req.Repository, req.PullID),
		URL: created.HTMLURL, Operation: "review-thread-reply",
		Fields: map[string]FieldDigest{"body": {After: digestString(req.Body)}},
	})
	return PullRequestInlineComment{ID: created.ID, Body: created.Body, InReplyTo: created.InReplyTo}, nil
}

// ResolvePullRequestReviewThread resolves one GraphQL review-thread node.
func (p *GitHubProvider) ResolvePullRequestReviewThread(ctx context.Context, repo RepositoryRef, threadID string) error {
	if err := requireOwnerRepo(repo); err != nil {
		return err
	}
	if strings.TrimSpace(threadID) == "" {
		return fmt.Errorf("review thread id is required")
	}
	var result struct {
		ResolveReviewThread struct {
			Thread struct {
				ID         string `json:"id"`
				IsResolved bool   `json:"isResolved"`
			} `json:"thread"`
		} `json:"resolveReviewThread"`
	}
	if err := p.graphql(ctx, resolveReviewThreadMutation, map[string]interface{}{"threadId": threadID}, &result); err != nil {
		return err
	}
	if result.ResolveReviewThread.Thread.ID != threadID || !result.ResolveReviewThread.Thread.IsResolved {
		return fmt.Errorf("github did not confirm review thread %q as resolved", threadID)
	}
	p.recordExternalRef(ctx, ExternalRef{
		Provider: ProviderGitHub, Ref: issueRef(repo, threadID), Operation: "review-thread-resolve",
	})
	return nil
}

var _ PullRequestReviewThreadProvider = (*GitHubProvider)(nil)
var _ PullRequestReviewThreadMutator = (*GitHubProvider)(nil)
