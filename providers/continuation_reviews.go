package providers

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/goobers/goobers/internal/mutationreceipt"
)

func (c *restMutationClient) review(ctx context.Context, req PullRequestReviewRequest, attribution Attribution) (PullRequestReviewResult, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return PullRequestReviewResult{}, err
	}
	if req.PullID == "" || req.CommitSHA == "" || req.Body == "" {
		return PullRequestReviewResult{}, fmt.Errorf("pull id, commit sha, and review body are required")
	}
	event, err := continuationReviewEvent(c.kind, req.Decision)
	if err != nil {
		return PullRequestReviewResult{}, err
	}
	identity, err := c.identity(ctx, req.Repository, "pull-request-review", "pulls/"+req.PullID+"/reviews", struct {
		Body, CommitSHA string
		Decision        ReviewDecision
	}{StripAttribution(req.Body), req.CommitSHA, req.Decision})
	if err != nil {
		return PullRequestReviewResult{}, err
	}
	endpoint, err := joinURL(c.baseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls", req.PullID, "reviews")
	if err != nil {
		return PullRequestReviewResult{}, err
	}
	var result PullRequestReviewResult
	err = c.session.Execute(ctx, identity, func(ctx context.Context, receipts []mutationreceipt.Receipt) (*mutationreceipt.Receipt, error) {
		reviews, err := c.reviews(ctx, req.Repository, req.PullID)
		if err != nil {
			return nil, err
		}
		actor, err := c.provider.AuthenticatedLogin(ctx)
		if err != nil {
			return nil, err
		}
		for _, receipt := range receipts {
			var found *PullRequestNativeReview
			for _, review := range reviews {
				if !strings.EqualFold(review.Author, actor) || !containsExactLine(review.Body, continuationMarker(receipt)) {
					continue
				}
				if review.ID <= 0 || review.CommitSHA != req.CommitSHA || continuationBody(review.Body) != StripAttribution(req.Body) || !continuationReviewMatches(review.State, req.Decision) || found != nil {
					return nil, fmt.Errorf("%w: conflicting review evidence", ErrMutationUnresolved)
				}
				copy := review
				found = &copy
			}
			if found != nil {
				result = PullRequestReviewResult{ID: found.ID, URL: found.URL, CommitSHA: found.CommitSHA, Decision: req.Decision}
				return &receipt, nil
			}
		}
		return nil, nil
	}, func(ctx context.Context, receipt mutationreceipt.Receipt) error {
		body, err := stampContinuationBody(req.Body, attribution, "pull-request-review", receipt)
		if err != nil {
			return err
		}
		var response restReviewResponse
		if err := c.provider.do(ctx, http.MethodPost, endpoint, map[string]string{"body": body, "commit_id": req.CommitSHA, "event": event}, &response); err != nil {
			return err
		}
		if response.ID <= 0 {
			return fmt.Errorf("%w: review response lacks identity", ErrMutationUnresolved)
		}
		result = PullRequestReviewResult{ID: response.ID, URL: response.HTMLURL, CommitSHA: req.CommitSHA, Decision: req.Decision}
		c.provider.recordExternalRef(ctx, ExternalRef{Provider: c.kind, Ref: issueRef(req.Repository, req.PullID), URL: response.HTMLURL, Operation: "review"})
		return nil
	})
	return result, err
}

func continuationReviewEvent(kind ProviderKind, decision ReviewDecision) (string, error) {
	switch decision {
	case ReviewDecisionApproved:
		if kind == ProviderGitea {
			return "APPROVED", nil
		}
		return "APPROVE", nil
	case ReviewDecisionChangesRequested:
		return "REQUEST_CHANGES", nil
	case ReviewDecisionComment:
		return "COMMENT", nil
	default:
		return "", fmt.Errorf("unsupported review decision %q", decision)
	}
}
func continuationReviewMatches(state string, decision ReviewDecision) bool {
	switch decision {
	case ReviewDecisionApproved:
		return state == "APPROVED"
	case ReviewDecisionChangesRequested:
		return state == "CHANGES_REQUESTED" || state == "REQUEST_CHANGES"
	case ReviewDecisionComment:
		return state == "COMMENTED" || state == "COMMENT"
	default:
		return false
	}
}

func (c *restMutationClient) reviews(ctx context.Context, repo RepositoryRef, pullID string) ([]PullRequestNativeReview, error) {
	if p, ok := c.provider.(*GitHubProvider); ok {
		return p.listNativePullRequestReviews(ctx, repo, pullID)
	}
	p, ok := c.provider.(*GiteaProvider)
	if !ok {
		return nil, fmt.Errorf("unsupported review reconciliation provider")
	}
	reviews, err := p.listGiteaPullReviews(ctx, repo, pullID)
	if err != nil {
		return nil, err
	}
	result := make([]PullRequestNativeReview, 0, len(reviews))
	for _, review := range reviews {
		result = append(result, PullRequestNativeReview{ID: review.ID, Body: review.Body, State: review.State, Author: review.User.Login, CommitSHA: review.CommitID, URL: review.HTMLURL})
	}
	return result, nil
}
