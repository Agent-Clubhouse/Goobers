package providers

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// InspectNeedsHuman reads one bounded comment and blocker page. It never follows
// pagination or cross-repository blocker links implicitly.
func (p *GitHubProvider) InspectNeedsHuman(ctx context.Context, repo RepositoryRef, id string) (NeedsHumanInspection, error) {
	var result NeedsHumanInspection
	if !nativePositiveID(id) {
		return result, ErrAttentionChanged
	}
	ctx, cancel := attentionContext(ctx)
	defer cancel()
	item, err := p.GetWorkItem(ctx, repo, id)
	if err != nil {
		return result, err
	}
	if !githubAttentionOwns(repo, item) {
		return result, ErrAttentionChanged
	}
	result.Item = item
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues", id, "comments")
	if err != nil {
		return result, err
	}
	endpoint, err = addQuery(endpoint, url.Values{"per_page": []string{strconv.Itoa(MaxAttentionComments)}, "page": []string{"1"}})
	if err != nil {
		return result, err
	}
	response, err := p.send(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return result, err
	}
	more := parseNextLink(response.Header.Get("Link")) != ""
	var comments []restComment
	if err = readJSONResponse(response, http.MethodGet, endpoint, &comments); err != nil {
		return result, err
	}
	if len(comments) > MaxAttentionComments {
		comments = comments[:MaxAttentionComments]
		more = true
	}
	for _, comment := range comments {
		result.Comments = append(result.Comments, mapGitHubComment(comment))
	}
	result.CommentsComplete = !more && attentionCommentBounds(result.Comments)
	if !attentionCommentBounds(result.Comments) {
		result.Comments = nil
	}
	result.Blockers, result.BlockersComplete, err = p.attentionBlockers(ctx, repo, id)
	return result, err
}
func (p *GitHubProvider) attentionBlockers(ctx context.Context, repo RepositoryRef, id string) ([]AttentionBlocker, bool, error) {
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues", id, "dependencies", "blocked_by")
	if err != nil {
		return nil, false, err
	}
	endpoint, err = addQuery(endpoint, url.Values{"per_page": []string{strconv.Itoa(MaxAttentionBlockers)}, "page": []string{"1"}})
	if err != nil {
		return nil, false, err
	}
	response, err := p.send(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, false, err
	}
	complete := parseNextLink(response.Header.Get("Link")) == ""
	var issues []githubIssue
	if err = readJSONResponse(response, http.MethodGet, endpoint, &issues); err != nil {
		return nil, false, err
	}
	if len(issues) > MaxAttentionBlockers {
		issues = issues[:MaxAttentionBlockers]
		complete = false
	}
	var blockers []AttentionBlocker
	for _, raw := range issues {
		native := mapGitHubIssue(raw)
		open, verified := attentionOpenState(native.State)
		own := githubAttentionOwns(repo, native)
		verified = verified && own && raw.PullRequest == nil && nativePositiveID(native.ID) && nativePositiveID(native.StableID)
		blocker := AttentionBlocker{ID: native.ID, Open: open, Verified: verified}
		if verified {
			blocker.StableID = native.StableID
			blocker.Revision = native.Revision
		} else {
			complete = false
			blocker.Open = true
		}
		blockers = append(blockers, blocker)
	}
	return blockers, complete, nil
}

// ClearNeedsHuman performs one exact label DELETE after timestamp preflight.
// GitHub has no atomic revision condition for this operation; the host reports
// that limitation and separately verifies the resulting item state.
func (p *GitHubProvider) ClearNeedsHuman(ctx context.Context, request NeedsHumanClearRequest) (NativeWorkItemPatchResult, error) {
	var result NativeWorkItemPatchResult
	if err := validateAttentionRequest(request); err != nil {
		return result, err
	}
	ctx, cancel := attentionContext(ctx)
	defer cancel()
	item, err := p.GetWorkItem(ctx, request.Repository, request.ID)
	if err != nil {
		return result, err
	}
	if !githubAttentionOwns(request.Repository, item) {
		return result, ErrAttentionChanged
	}
	if err = validateAttentionIdentity(item, request); err != nil {
		return result, err
	}
	endpoint, err := joinURL(p.BaseURL, "repos", request.Repository.Owner, request.Repository.Name, "issues", request.ID, "labels", LabelNeedsHuman)
	if err != nil {
		return result, err
	}
	result.MutationAttempted = true
	if err = p.do(WithoutMutationRetries(ctx), http.MethodDelete, endpoint, nil, nil); err != nil {
		return result, err
	}
	result.Acknowledged = true
	p.recordExternalRef(ctx, ExternalRef{Provider: ProviderGitHub, Ref: issueRef(request.Repository, request.ID), URL: item.URL, Operation: "needs-human-resolve"})
	return result, nil
}

func githubAttentionOwns(repo RepositoryRef, item WorkItem) bool {
	parsed, err := url.Parse(item.URL)
	raw, ok := item.Raw.(githubIssue)
	return ok && raw.PullRequest == nil && err == nil && parsed.Scheme == "https" && strings.EqualFold(parsed.Host, "github.com") && strings.EqualFold(strings.TrimSuffix(parsed.Path, "/"), "/"+repo.Owner+"/"+repo.Name+"/issues/"+item.ID) && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == ""
}
