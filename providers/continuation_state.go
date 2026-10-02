package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/goobers/goobers/internal/mutationreceipt"
)

func (c *restMutationClient) patch(ctx context.Context, repo RepositoryRef, resource, id string, patch map[string]interface{}, out any) error {
	if id == "" {
		return fmt.Errorf("provider mutation target id is required")
	}
	identity, err := c.identity(ctx, repo, "patch", resource+"/"+id, patch)
	if err != nil {
		return err
	}
	endpoint, err := joinURL(c.baseURL, "repos", repo.Owner, repo.Name, resource, id)
	if err != nil {
		return err
	}
	return c.session.Execute(ctx, identity, func(ctx context.Context, receipts []mutationreceipt.Receipt) (*mutationreceipt.Receipt, error) {
		var current map[string]json.RawMessage
		if err := c.provider.do(ctx, http.MethodGet, endpoint, nil, &current); err != nil {
			return nil, err
		}
		for field, want := range patch {
			match, err := continuationFieldMatches(c.kind, field, want, current[field])
			if err != nil {
				return nil, err
			}
			if !match {
				return nil, nil
			}
		}
		if out != nil {
			encoded, err := json.Marshal(current)
			if err != nil {
				return nil, err
			}
			if err := json.Unmarshal(encoded, out); err != nil {
				return nil, err
			}
		}
		return &receipts[0], nil
	}, func(ctx context.Context, _ mutationreceipt.Receipt) error {
		return c.provider.do(ctx, http.MethodPatch, endpoint, patch, out)
	})
}

func continuationFieldMatches(kind ProviderKind, field string, want any, raw json.RawMessage) (bool, error) {
	if len(raw) == 0 {
		return false, nil
	}
	switch field {
	case "title", "body", "state":
		var got string
		if err := json.Unmarshal(raw, &got); err != nil {
			return false, err
		}
		expected, ok := want.(string)
		return ok && got == expected, nil
	case "assignees":
		var got []githubUser
		if err := json.Unmarshal(raw, &got); err != nil {
			return false, err
		}
		expected, ok := want.([]string)
		if !ok {
			return false, nil
		}
		actual := make([]string, 0, len(got))
		normalized := make([]string, 0, len(expected))
		for _, user := range got {
			actual = append(actual, strings.ToLower(user.Login))
		}
		for _, user := range expected {
			normalized = append(normalized, strings.ToLower(user))
		}
		sort.Strings(actual)
		sort.Strings(normalized)
		return slices.Equal(actual, normalized), nil
	case "milestone":
		var got struct {
			ID     int64 `json:"id"`
			Number int64 `json:"number"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			return false, err
		}
		value := got.Number
		if kind == ProviderGitea {
			value = got.ID
		}
		expected, ok := want.(int)
		return ok && value == int64(expected), nil
	default:
		return false, fmt.Errorf("%w: unsupported field evidence", ErrMutationUnresolved)
	}
}

func (c *restMutationClient) labels(ctx context.Context, repo RepositoryRef, id string, add, remove []string) error {
	if id == "" {
		return errIssueIDRequired
	}
	for _, label := range uniqueStrings(add) {
		if err := c.label(ctx, repo, id, label, true); err != nil {
			return err
		}
	}
	for _, label := range uniqueStrings(remove) {
		if err := c.label(ctx, repo, id, label, false); err != nil {
			return err
		}
	}
	return nil
}

func (c *restMutationClient) label(ctx context.Context, repo RepositoryRef, id, label string, add bool) error {
	action := "label-remove"
	if add {
		action = "label-add"
	}
	identity, err := c.identity(ctx, repo, action, "issues/"+id+"/labels", strings.ToLower(label))
	if err != nil {
		return err
	}
	endpoint, err := joinURL(c.baseURL, "repos", repo.Owner, repo.Name, "issues", id, "labels")
	if err != nil {
		return err
	}
	return c.session.Execute(ctx, identity, func(ctx context.Context, receipts []mutationreceipt.Receipt) (*mutationreceipt.Receipt, error) {
		present := false
		err := c.provider.getAllPages(ctx, endpoint, func(page []byte) error {
			var labels []githubLabel
			if err := json.Unmarshal(page, &labels); err != nil {
				return err
			}
			for _, got := range labels {
				if strings.EqualFold(got.Name, label) {
					present = true
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if present == add {
			return &receipts[0], nil
		}
		return nil, nil
	}, func(ctx context.Context, _ mutationreceipt.Receipt) error {
		return c.writeLabel(ctx, repo, endpoint, label, add)
	})
}

func (c *restMutationClient) writeLabel(ctx context.Context, repo RepositoryRef, endpoint, label string, add bool) error {
	switch p := c.provider.(type) {
	case *GitHubProvider:
		if add {
			return p.do(ctx, http.MethodPost, endpoint, map[string][]string{"labels": {label}}, nil)
		}
		path, err := joinURL(endpoint, label)
		if err != nil {
			return err
		}
		return doStatus(ctx, p, http.MethodDelete, path, nil, nil, []int{http.StatusNotFound})
	case *GiteaProvider:
		if add {
			ids, err := p.giteaLabelIDs(mutationreceipt.FreshRead(ctx), repo, []string{label})
			if err != nil {
				return err
			}
			return p.do(ctx, http.MethodPost, endpoint, map[string][]int64{"labels": ids}, nil)
		}
		ids, err := p.resolveExistingLabelIDs(mutationreceipt.FreshRead(ctx), repo, []string{label})
		if err != nil {
			return err
		}
		for _, id := range ids {
			path, err := joinURL(endpoint, strconv.FormatInt(id, 10))
			if err != nil {
				return err
			}
			if err := doStatus(ctx, p, http.MethodDelete, path, nil, nil, []int{http.StatusNotFound}); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported label reconciliation provider")
	}
}

func closeContinuationPull(ctx context.Context, c restDoer, req ClosePullRequestRequest, endpoint string, out *restClosedPull) error {
	if client := continuationClient(c); client != nil {
		return client.patch(ctx, req.Repository, "pulls", req.PullID, map[string]interface{}{"state": "closed"}, out)
	}
	return c.do(ctx, http.MethodPatch, endpoint, map[string]string{"state": "closed"}, out)
}
