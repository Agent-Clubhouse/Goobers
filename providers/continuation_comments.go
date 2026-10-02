package providers

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/goobers/goobers/internal/mutationreceipt"
)

var continuationMarkerPattern = regexp.MustCompile(`\n*<!-- goobers:mutation v1 [A-Za-z0-9_-]+ -->\n?`)

func continuationMarker(receipt mutationreceipt.Receipt) string {
	return "<!-- goobers:mutation v1 " + base64.RawURLEncoding.EncodeToString([]byte(receipt.ID)) + " -->"
}
func continuationBody(body string) string {
	return StripAttribution(continuationMarkerPattern.ReplaceAllString(body, "\n"))
}
func stampContinuationBody(body string, attribution Attribution, action string, receipt mutationreceipt.Receipt) (string, error) {
	if strings.Contains(body, "<!-- goobers:mutation") {
		return "", fmt.Errorf("provider body already contains a reserved mutation marker")
	}
	stamped, err := withAttribution(body, attribution, action)
	if err != nil {
		return "", err
	}
	return stamped + "\n\n" + continuationMarker(receipt), nil
}

func (c *restMutationClient) comment(ctx context.Context, repo RepositoryRef, id, body, action string, attribution Attribution) (restComment, error) {
	if id == "" {
		return restComment{}, errIssueIDRequired
	}
	identity, err := c.identity(ctx, repo, action, "issues/"+id+"/comments", StripAttribution(body))
	if err != nil {
		return restComment{}, err
	}
	endpoint, err := joinURL(c.baseURL, "repos", repo.Owner, repo.Name, "issues", id, "comments")
	if err != nil {
		return restComment{}, err
	}
	var result restComment
	err = c.session.Execute(ctx, identity, func(ctx context.Context, receipts []mutationreceipt.Receipt) (*mutationreceipt.Receipt, error) {
		comments, err := allIssueComments(ctx, c.provider, c.baseURL, repo, id)
		if err != nil {
			return nil, err
		}
		actor, err := c.provider.AuthenticatedLogin(ctx)
		if err != nil {
			return nil, err
		}
		for _, receipt := range receipts {
			var found *restComment
			for _, comment := range comments {
				if !strings.EqualFold(comment.User.Login, actor) || !containsExactLine(comment.Body, continuationMarker(receipt)) {
					continue
				}
				if comment.ID <= 0 || continuationBody(comment.Body) != StripAttribution(body) || found != nil {
					return nil, fmt.Errorf("%w: conflicting comment evidence", ErrMutationUnresolved)
				}
				copy := comment
				found = &copy
			}
			if found != nil {
				result = *found
				return &receipt, nil
			}
		}
		return nil, nil
	}, func(ctx context.Context, receipt mutationreceipt.Receipt) error {
		stamped, err := stampContinuationBody(body, attribution, action, receipt)
		if err != nil {
			return err
		}
		if err := c.provider.do(ctx, http.MethodPost, endpoint, map[string]string{"body": stamped}, &result); err != nil {
			return err
		}
		if result.ID <= 0 {
			return fmt.Errorf("%w: comment response lacks identity", ErrMutationUnresolved)
		}
		return nil
	})
	if err != nil {
		return restComment{}, err
	}
	return result, nil
}

func (c *restMutationClient) updateComment(ctx context.Context, repo RepositoryRef, id, body string, attribution Attribution) error {
	if id == "" {
		return fmt.Errorf("comment id is required")
	}
	identity, err := c.identity(ctx, repo, "comment-update", "issues/comments/"+id, StripAttribution(body))
	if err != nil {
		return err
	}
	endpoint, err := joinURL(c.baseURL, "repos", repo.Owner, repo.Name, "issues", "comments", id)
	if err != nil {
		return err
	}
	return c.session.Execute(ctx, identity, func(ctx context.Context, receipts []mutationreceipt.Receipt) (*mutationreceipt.Receipt, error) {
		var current restComment
		if err := c.provider.do(ctx, http.MethodGet, endpoint, nil, &current); err != nil {
			return nil, err
		}
		actor, err := c.provider.AuthenticatedLogin(ctx)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(current.User.Login, actor) || continuationBody(current.Body) != StripAttribution(body) {
			return nil, nil
		}
		for _, receipt := range receipts {
			if containsExactLine(current.Body, continuationMarker(receipt)) {
				return &receipt, nil
			}
		}
		return nil, nil
	}, func(ctx context.Context, receipt mutationreceipt.Receipt) error {
		stamped, err := stampContinuationBody(body, attribution, "comment-update", receipt)
		if err != nil {
			return err
		}
		var updated restComment
		if err := c.provider.do(ctx, http.MethodPatch, endpoint, map[string]string{"body": stamped}, &updated); err != nil {
			return err
		}
		if ref, ok := commentMutationRef(c.kind, repo, updated); ok {
			c.provider.recordExternalRef(ctx, ref)
		}
		return nil
	})
}
