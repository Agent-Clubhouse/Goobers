package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/goobers/goobers/internal/mutationreceipt"
)

var continuationRunFooter = regexp.MustCompile(`(?:\n\n)?---\ngoobers run-id: [^\n]+$`)

// openPull has one branch-scoped semantic intent across create and reuse. A
// lost create response must not switch to an unrelated PATCH intent on replay.
func (c *restMutationClient) openPull(ctx context.Context, req PullRequestRequest) (PullRequestResult, error) {
	if req.Head == "" || req.Base == "" {
		return PullRequestResult{}, fmt.Errorf("head and base branches are required")
	}
	title := req.Title
	if c.kind == ProviderGitea && req.Draft {
		title = "WIP: " + title
	}
	identity, err := c.identity(ctx, req.Repository, "pull-request-open", "pulls/head/"+req.Head+"/base/"+req.Base, struct {
		Title, Body, Head, Base string
		Draft                   bool
	}{title, StripAttribution(req.Body), req.Head, req.Base, req.Draft})
	if err != nil {
		return PullRequestResult{}, err
	}
	endpoint, err := joinURL(c.baseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls")
	if err != nil {
		return PullRequestResult{}, err
	}
	var result PullRequestResult
	err = c.session.Execute(ctx, identity, func(ctx context.Context, receipts []mutationreceipt.Receipt) (*mutationreceipt.Receipt, error) {
		pulls, err := c.branchPulls(ctx, req)
		if err != nil {
			return nil, err
		}
		for _, receipt := range receipts {
			var found *continuationPull
			for _, pull := range pulls {
				if !containsExactLine(pull.Body, continuationMarker(receipt)) {
					continue
				}
				semanticBody := continuationRunFooter.ReplaceAllString(continuationBody(pull.Body), "")
				if found != nil || pull.Number <= 0 || pull.State != "open" || pull.Title != title || semanticBody != StripAttribution(req.Body) || (c.kind == ProviderGitHub && pull.Draft != req.Draft) {
					return nil, fmt.Errorf("%w: conflicting pull request evidence", ErrMutationUnresolved)
				}
				copy := pull
				found = &copy
			}
			if found != nil {
				result = continuationPullResult(*found)
				return &receipt, nil
			}
		}
		return nil, nil
	}, func(ctx context.Context, receipt mutationreceipt.Receipt) error {
		result, err = c.writeOpenPull(ctx, req, title, endpoint, receipt)
		return err
	})
	return result, err
}

func continuationPullResult(pull continuationPull) PullRequestResult {
	return PullRequestResult{ID: strconv.Itoa(pull.Number), Number: pull.Number, URL: pull.URL}
}

func (c *restMutationClient) branchPulls(ctx context.Context, req PullRequestRequest) ([]continuationPull, error) {
	endpoint, err := joinURL(c.baseURL, "repos", req.Repository.Owner, req.Repository.Name, "pulls")
	if err != nil {
		return nil, err
	}
	endpoint, err = addQuery(endpoint, url.Values{"state": {"all"}})
	if err != nil {
		return nil, err
	}
	var matches []continuationPull
	err = c.provider.getAllPages(mutationreceipt.FreshRead(ctx), endpoint, func(page []byte) error {
		var pulls []continuationPull
		if err := json.Unmarshal(page, &pulls); err != nil {
			return err
		}
		for _, pull := range pulls {
			if pull.Head.Ref != req.Head || pull.Base.Ref != req.Base {
				continue
			}
			// Same branch spelling in a fork is a different creation target.
			if pull.Head.Repo == nil {
				return fmt.Errorf("%w: matching branch lacks repository identity", ErrMutationUnresolved)
			}
			if !strings.EqualFold(pull.Head.Repo.Name, req.Repository.Name) || !strings.EqualFold(pull.Head.Repo.Owner.Login, req.Repository.Owner) {
				continue
			}
			matches = append(matches, pull)
		}
		return nil
	})
	return matches, err
}

func (c *restMutationClient) writeOpenPull(ctx context.Context, req PullRequestRequest, title, endpoint string, receipt mutationreceipt.Receipt) (PullRequestResult, error) {
	pulls, err := c.branchPulls(ctx, req)
	if err != nil {
		return PullRequestResult{}, err
	}
	var existing *continuationPull
	for _, pull := range pulls {
		if pull.State != "open" {
			continue
		}
		if existing != nil {
			return PullRequestResult{}, fmt.Errorf("%w: multiple open branch matches", ErrMutationUnresolved)
		}
		copy := pull
		existing = &copy
	}
	body, err := stampContinuationBody(withRunIDFooter(req.Body, req.RunID), Attribution{}, "pull-request-open", receipt)
	if err != nil {
		return PullRequestResult{}, err
	}
	payload := map[string]interface{}{"title": title, "body": body}
	method := http.MethodPost
	target := endpoint
	if existing == nil {
		payload["head"] = req.Head
		payload["base"] = req.Base
		if c.kind == ProviderGitHub {
			payload["draft"] = req.Draft
		}
	} else {
		method = http.MethodPatch
		target, err = joinURL(endpoint, strconv.Itoa(existing.Number))
		if err != nil {
			return PullRequestResult{}, err
		}
		// REST cannot change GitHub draft state; do not claim this semantic input
		// completed if the existing PR differs from the requested draft state.
		if c.kind == ProviderGitHub && existing.Draft != req.Draft {
			return PullRequestResult{}, fmt.Errorf("%w: existing pull draft state differs", ErrMutationUnresolved)
		}
	}
	var out continuationPull
	if err := c.provider.do(ctx, method, target, payload, &out); err != nil {
		return PullRequestResult{}, err
	}
	if out.Number <= 0 || (existing != nil && out.Number != existing.Number) {
		return PullRequestResult{}, fmt.Errorf("%w: pull response lacks identity", ErrMutationUnresolved)
	}
	result := continuationPullResult(out)
	operation := "open"
	if existing != nil {
		operation = "update"
	}
	c.provider.recordExternalRef(ctx, ExternalRef{Provider: c.kind, Ref: issueRef(req.Repository, result.ID), URL: result.URL, Operation: operation, RunID: req.RunID})
	return result, nil
}
