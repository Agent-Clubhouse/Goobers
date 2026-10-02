package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/goobers/goobers/internal/mutationreceipt"
)

func (c *restMutationClient) deleteComment(ctx context.Context, repo RepositoryRef, id string) error {
	if id == "" {
		return fmt.Errorf("comment id is required")
	}
	identity, err := c.identity(ctx, repo, "comment-delete", "issues/comments/"+id, nil)
	if err != nil {
		return err
	}
	endpoint, err := joinURL(c.baseURL, "repos", repo.Owner, repo.Name, "issues", "comments", id)
	if err != nil {
		return err
	}
	return c.session.Execute(ctx, identity, func(ctx context.Context, receipts []mutationreceipt.Receipt) (*mutationreceipt.Receipt, error) {
		err := c.provider.do(ctx, http.MethodGet, endpoint, nil, nil)
		if IsNotFoundError(err) {
			return &receipts[0], nil
		}
		return nil, err
	}, func(ctx context.Context, _ mutationreceipt.Receipt) error {
		err := c.provider.do(ctx, http.MethodDelete, endpoint, nil, nil)
		if IsNotFoundError(err) {
			return nil
		}
		return err
	})
}

type continuationPull struct {
	Number int        `json:"number"`
	Title  string     `json:"title"`
	Body   string     `json:"body"`
	State  string     `json:"state"`
	Draft  bool       `json:"draft"`
	URL    string     `json:"html_url"`
	User   githubUser `json:"user"`
	Head   struct {
		Ref  string          `json:"ref"`
		SHA  string          `json:"sha"`
		Repo *restRepository `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	RequestedReviewers []githubUser `json:"requested_reviewers"`
}

func (c *restMutationClient) pull(ctx context.Context, repo RepositoryRef, id string) (continuationPull, error) {
	var out continuationPull
	number, err := strconv.Atoi(id)
	if err != nil || number < 1 {
		return out, errPullIDRequired
	}
	if err := requireOwnerRepo(repo); err != nil {
		return out, err
	}
	endpoint, err := joinURL(c.baseURL, "repos", repo.Owner, repo.Name, "pulls", id)
	if err != nil {
		return out, err
	}
	err = c.provider.do(mutationreceipt.FreshRead(ctx), http.MethodGet, endpoint, nil, &out)
	if err == nil && (out.Number != number || out.Head.SHA == "") {
		err = fmt.Errorf("%w: pull response lacks identity/head", ErrMutationUnresolved)
	}
	return out, err
}

func (c *restMutationClient) requestReview(ctx context.Context, req ReviewRequest) error {
	pull, err := c.pull(ctx, req.Repository, req.PullID)
	if err != nil {
		return err
	}
	reviewers := uniqueStrings(req.Reviewers)
	sort.Strings(reviewers)
	for _, reviewer := range reviewers {
		if err := c.requestReviewer(ctx, req, pull.Head.SHA, reviewer); err != nil {
			return err
		}
	}
	return nil
}

func (c *restMutationClient) requestReviewer(ctx context.Context, req ReviewRequest, head, reviewer string) error {
	identity, err := c.identity(ctx, req.Repository, "request-review", "pulls/"+req.PullID, struct{ Head, Reviewer string }{head, strings.ToLower(reviewer)})
	if err != nil {
		return err
	}
	endpoint, err := joinURL(c.baseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls", req.PullID, "requested_reviewers")
	if err != nil {
		return err
	}
	return c.session.Execute(ctx, identity, func(ctx context.Context, receipts []mutationreceipt.Receipt) (*mutationreceipt.Receipt, error) {
		pull, err := c.pull(ctx, req.Repository, req.PullID)
		if err != nil {
			return nil, err
		}
		if pull.Head.SHA != head {
			return nil, nil
		}
		for _, user := range pull.RequestedReviewers {
			if strings.EqualFold(user.Login, reviewer) {
				return &receipts[0], nil
			}
		}
		// A vanished request may have been completed or removed by a human. Never
		// re-request from absence, even when the original write's outcome is unknown.
		return nil, nil
	}, func(ctx context.Context, _ mutationreceipt.Receipt) error {
		if err := c.provider.do(ctx, http.MethodPost, endpoint, map[string][]string{"reviewers": {reviewer}}, nil); err != nil {
			return err
		}
		c.provider.recordExternalRef(ctx, ExternalRef{Provider: c.kind, Ref: issueRef(req.Repository, req.PullID), Operation: "request-review"})
		return nil
	})
}

func (c *restMutationClient) ensureLabels(ctx context.Context, repo RepositoryRef, labels []WorkItemLabel) (EnsureWorkItemLabelsResult, error) {
	result := EnsureWorkItemLabelsResult{Created: []string{}, Skipped: []string{}}
	for _, label := range labels {
		created, err := c.ensureLabel(ctx, repo, label)
		if err != nil {
			return result, err
		}
		if created {
			result.Created = append(result.Created, strings.TrimSpace(label.Name))
		} else {
			result.Skipped = append(result.Skipped, strings.TrimSpace(label.Name))
		}
	}
	return result, nil
}

func (c *restMutationClient) repoLabel(ctx context.Context, repo RepositoryRef, name string) (*giteaLabel, error) {
	endpoint, err := joinURL(c.baseURL, "repos", repo.Owner, repo.Name, "labels")
	if err != nil {
		return nil, err
	}
	var found *giteaLabel
	err = c.provider.getAllPages(mutationreceipt.FreshRead(ctx), endpoint, func(page []byte) error {
		var labels []giteaLabel
		if err := json.Unmarshal(page, &labels); err != nil {
			return err
		}
		for _, label := range labels {
			if strings.EqualFold(label.Name, name) {
				if found != nil {
					return fmt.Errorf("%w: duplicate label identity", ErrMutationUnresolved)
				}
				copy := label
				found = &copy
			}
		}
		return nil
	})
	return found, err
}

func (c *restMutationClient) ensureLabel(ctx context.Context, repo RepositoryRef, label WorkItemLabel) (bool, error) {
	label.Name = strings.TrimSpace(label.Name)
	label.Color = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(label.Color), "#"))
	if label.Name == "" || label.Color == "" {
		return false, fmt.Errorf("label name and color are required")
	}
	identity, err := c.identity(ctx, repo, "label-ensure", "labels/"+strings.ToLower(label.Name), label)
	if err != nil {
		return false, err
	}
	endpoint, err := joinURL(c.baseURL, "repos", repo.Owner, repo.Name, "labels")
	if err != nil {
		return false, err
	}
	created := false
	err = c.session.Execute(ctx, identity, func(ctx context.Context, receipts []mutationreceipt.Receipt) (*mutationreceipt.Receipt, error) {
		existing, err := c.repoLabel(ctx, repo, label.Name)
		// Ensure preserves existing definitions, including concurrent human edits.
		if err == nil && existing != nil {
			return &receipts[0], nil
		}
		return nil, err
	}, func(ctx context.Context, _ mutationreceipt.Receipt) error {
		existing, err := c.repoLabel(ctx, repo, label.Name)
		if err != nil || existing != nil {
			return err
		}
		var out giteaLabel
		if err := c.provider.do(ctx, http.MethodPost, endpoint, map[string]string{"name": label.Name, "color": label.Color, "description": label.Description}, &out); err != nil {
			return err
		}
		if out.ID <= 0 || !strings.EqualFold(out.Name, label.Name) {
			return fmt.Errorf("%w: label response lacks identity", ErrMutationUnresolved)
		}
		created = true
		return nil
	})
	return created, err
}
