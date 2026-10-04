package providers

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// InspectNeedsHuman reads one bounded comment page and the item's predecessor
// relations. A foreign, missing or unreadable predecessor remains unresolved.
func (p *ADOProvider) InspectNeedsHuman(ctx context.Context, repo RepositoryRef, id string) (NeedsHumanInspection, error) {
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
	if !strings.EqualFold(stringField(item.Fields, "System.TeamProject"), p.project(repo)) {
		return result, ErrAttentionChanged
	}
	result.Item = item
	endpoint, err := p.workURLVersion(p.project(repo), "7.1-preview.4", "workItems", id, "comments")
	if err != nil {
		return result, err
	}
	endpoint, err = addQuery(endpoint, url.Values{"$top": []string{strconv.Itoa(MaxAttentionComments)}, "order": []string{"asc"}})
	if err != nil {
		return result, err
	}
	response, err := p.send(ctx, http.MethodGet, endpoint, nil, "")
	if err != nil {
		return result, err
	}
	next := strings.TrimSpace(response.Header.Get("x-ms-continuationtoken"))
	var page adoCommentsResponse
	if err = readJSONResponse(response, http.MethodGet, endpoint, &page); err != nil {
		return result, err
	}
	if page.ContinuationToken != "" {
		next = page.ContinuationToken
	}
	if len(page.Comments) > MaxAttentionComments {
		page.Comments = page.Comments[:MaxAttentionComments]
		next = "bounded"
	}
	for _, comment := range page.Comments {
		result.Comments = append(result.Comments, mapADOComment(comment))
	}
	result.CommentsComplete = next == "" && attentionCommentBounds(result.Comments)
	if !attentionCommentBounds(result.Comments) {
		result.Comments = nil
	}
	result.Blockers, result.BlockersComplete, err = p.attentionPredecessors(ctx, repo, item)
	return result, err
}
func (p *ADOProvider) attentionPredecessors(ctx context.Context, repo RepositoryRef, item WorkItem) ([]AttentionBlocker, bool, error) {
	raw, err := rawADOWorkItem(item)
	if err != nil {
		return nil, false, err
	}
	ids, err := adoPredecessorIDs(raw.Relations)
	if err != nil {
		return nil, false, err
	}
	complete := len(ids) <= MaxAttentionBlockers
	if !complete {
		ids = ids[:MaxAttentionBlockers]
	}
	if len(ids) == 0 {
		return nil, complete, nil
	}
	items, err := p.getWorkItemsBatch(ctx, repo, ids)
	if err != nil {
		return nil, false, err
	}
	byID := make(map[int]adoWorkItem, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	blockers := make([]AttentionBlocker, 0, len(ids))
	for _, id := range ids {
		blocker := AttentionBlocker{ID: strconv.Itoa(id), Open: true}
		native, ok := byID[id]
		if !ok || !strings.EqualFold(stringField(native.Fields, "System.TeamProject"), p.project(repo)) {
			complete = false
			blockers = append(blockers, blocker)
			continue
		}
		open, err := p.adoPredecessorBlocks(ctx, repo, native)
		if err != nil {
			complete = false
			blockers = append(blockers, blocker)
			continue
		}
		blocker.Open = open
		blocker.Verified = true
		blocker.StableID = strconv.Itoa(id)
		blocker.Revision = strconv.Itoa(native.Rev)
		blockers = append(blockers, blocker)
	}
	return blockers, complete, nil
}

// ClearNeedsHuman uses one /rev-tested tags patch, preserving every other tag.
// It never alters workflow state, readiness, claim custody or assignments.
func (p *ADOProvider) ClearNeedsHuman(ctx context.Context, request NeedsHumanClearRequest) (NativeWorkItemPatchResult, error) {
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
	if !strings.EqualFold(stringField(item.Fields, "System.TeamProject"), p.project(request.Repository)) {
		return result, ErrAttentionChanged
	}
	if err = validateAttentionIdentity(item, request); err != nil {
		return result, err
	}
	raw, err := rawADOWorkItem(item)
	if err != nil {
		return result, err
	}
	tags := strings.Split(stringField(raw.Fields, "System.Tags"), ";")
	kept := make([]string, 0, len(tags))
	for _, tag := range tags {
		if strings.TrimSpace(tag) != "" && !strings.EqualFold(strings.TrimSpace(tag), LabelNeedsHuman) {
			kept = append(kept, strings.TrimSpace(tag))
		}
	}
	patch := []adoPatchOperation{{Op: "test", Path: "/rev", Value: raw.Rev}, {Op: "add", Path: "/fields/System.Tags", Value: strings.Join(kept, "; ")}}
	result.MutationAttempted = true
	result.Item, err = p.patchADOWorkItem(WithoutMutationRetries(ctx), request.Repository, request.ID, patch)
	if err != nil {
		return result, err
	}
	result.Acknowledged = true
	p.recordMutation(ctx, "issue", request.ID, "needs-human-resolve", request.Repository)
	return result, nil
}
