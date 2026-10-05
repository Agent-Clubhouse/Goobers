package providers

import (
	"context"
	"net/http"
)

// PatchNativeWorkItem sends one GitHub issue PATCH. ExpectedRevision is a
// timestamp preflight only: GitHub does not provide an atomic revision test here.
func (p *GitHubProvider) PatchNativeWorkItem(ctx context.Context, req NativeWorkItemPatch) (NativeWorkItemPatchResult, error) {
	var result NativeWorkItemPatchResult
	if err := requireOwnerRepo(req.Repository); err != nil {
		return result, err
	}
	if err := ValidateNativeWorkItemPatch(req, ProviderGitHub); err != nil {
		return result, err
	}
	current, err := p.GetWorkItem(ctx, req.Repository, req.ID)
	if err != nil {
		return result, err
	}
	if err = checkNativeEditIdentity(current, req); err != nil {
		return result, err
	}
	patch := map[string]interface{}{}
	switch req.Field {
	case "title", "state":
		patch[req.Field] = *req.Value
	case "description":
		patch["body"] = *req.Value
	case "labels", "assignees":
		patch[req.Field] = append([]string{}, req.Values...)
	}
	endpoint, err := joinURL(p.BaseURL, "repos", req.Repository.Owner, req.Repository.Name, "issues", req.ID)
	if err != nil {
		return result, err
	}
	result.MutationAttempted = true
	var out githubIssue
	if err = p.do(WithoutMutationRetries(ctx), http.MethodPatch, endpoint, patch, &out); err != nil {
		return result, err
	}
	result.Acknowledged = true
	result.Item = mapGitHubIssue(out)
	p.recordExternalRef(ctx, ExternalRef{Provider: ProviderGitHub, Ref: issueRef(req.Repository, req.ID), URL: result.Item.URL, Operation: "workbench-" + req.Field})
	return result, nil
}
