package providers

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// adoWorkItemsBatchSize is the most ids one workitemsbatch call accepts.
const adoWorkItemsBatchSize = 200

// adoWorkItemsBatchPath is the path suffix of the read-only hydration
// endpoint. A POST to it only reads, so send may resend it after a transient
// failure the same way it resends a GET.
const adoWorkItemsBatchPath = "/_apis/wit/workitemsbatch"

type adoWorkItemsBatchRequest struct {
	IDs    []int  `json:"ids"`
	Expand string `json:"$expand"`
	// ErrorPolicy "Omit" returns a null entry for an id that no longer
	// exists or cannot be read, instead of failing the whole batch.
	ErrorPolicy string `json:"errorPolicy"`
}

type adoWorkItemsBatchResponse struct {
	Value []*adoWorkItem `json:"value"`
}

// getWorkItemsBatch hydrates work items through POST _apis/wit/workitemsbatch
// with their relations expanded, at most adoWorkItemsBatchSize ids per call.
// Items come back in the order of ids; the response order is not relied on.
// An id the batch omits (deleted or unreadable since the WIQL query ran) is
// skipped.
func (p *ADOProvider) getWorkItemsBatch(ctx context.Context, repo RepositoryRef, ids []int) ([]adoWorkItem, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	project := p.project(repo)
	if err := p.requireWorkItemScope(project); err != nil {
		return nil, err
	}
	endpoint, err := p.workURL(project, "workitemsbatch")
	if err != nil {
		return nil, err
	}
	byID := make(map[int]adoWorkItem, len(ids))
	for start := 0; start < len(ids); start += adoWorkItemsBatchSize {
		body := adoWorkItemsBatchRequest{
			IDs:         ids[start:min(start+adoWorkItemsBatchSize, len(ids))],
			Expand:      "Relations",
			ErrorPolicy: "Omit",
		}
		var out adoWorkItemsBatchResponse
		if err := p.do(ctx, http.MethodPost, endpoint, body, &out); err != nil {
			return nil, err
		}
		for _, item := range out.Value {
			if item != nil {
				byID[item.ID] = *item
			}
		}
	}
	ordered := make([]adoWorkItem, 0, len(ids))
	for _, id := range ids {
		if item, ok := byID[id]; ok {
			ordered = append(ordered, item)
		}
	}
	return ordered, nil
}

// adoRefIDs returns the ids of WIQL hits in WIQL order.
func adoRefIDs(refs []adoWorkItemRef) []int {
	ids := make([]int, len(refs))
	for i, ref := range refs {
		ids[i] = ref.ID
	}
	return ids
}

// getWorkItemRefsBatch hydrates WIQL hits and indexes them by id, so a
// caller walking the hits in WIQL order can tell an omitted item apart.
func (p *ADOProvider) getWorkItemRefsBatch(ctx context.Context, repo RepositoryRef, refs []adoWorkItemRef) (map[int]adoWorkItem, error) {
	items, err := p.getWorkItemsBatch(ctx, repo, adoRefIDs(refs))
	if err != nil {
		return nil, err
	}
	byID := make(map[int]adoWorkItem, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	return byID, nil
}

// listWorkItemsBatch hydrates WIQL hits in WIQL order and maps each to a
// unified work item, skipping hits the batch omitted.
func (p *ADOProvider) listWorkItemsBatch(ctx context.Context, repo RepositoryRef, refs []adoWorkItemRef) ([]WorkItem, error) {
	raw, err := p.getWorkItemsBatch(ctx, repo, adoRefIDs(refs))
	if err != nil {
		return nil, err
	}
	items := make([]WorkItem, 0, len(raw))
	for _, item := range raw {
		mapped, err := p.mapADOWorkItem(ctx, repo, item)
		if err != nil {
			return nil, err
		}
		items = append(items, mapped)
	}
	return items, nil
}

// scanWorkItemCandidates hydrates ListWorkItems' WIQL hits one
// workitemsbatch chunk at a time and filters them in WIQL order. It returns
// the matches and the index of the last hit inspected, stopping once
// req.Limit matches are in hand (#2067) so later chunks are never fetched.
// A hit the batch omitted still counts as inspected.
func (p *ADOProvider) scanWorkItemCandidates(ctx context.Context, req ListWorkItemsRequest, requestedState string, refs []adoWorkItemRef) ([]WorkItem, int, error) {
	items := make([]WorkItem, 0, min(len(refs), adoWorkItemsBatchSize))
	lastScanned := -1
	for start := 0; start < len(refs); start += adoWorkItemsBatchSize {
		chunk := refs[start:min(start+adoWorkItemsBatchSize, len(refs))]
		hydrated, err := p.getWorkItemRefsBatch(ctx, req.Repository, chunk)
		if err != nil {
			return nil, lastScanned, err
		}
		for _, ref := range chunk {
			lastScanned++
			raw, ok := hydrated[ref.ID]
			if !ok {
				continue
			}
			item, err := p.mapADOWorkItem(ctx, req.Repository, raw)
			if err != nil {
				return nil, lastScanned, err
			}
			matched, err := adoListCandidateMatches(req, requestedState, item)
			if err != nil {
				return nil, lastScanned, err
			}
			if !matched {
				continue
			}
			items = append(items, item)
			if req.Limit > 0 && len(items) >= req.Limit {
				return items, lastScanned, nil
			}
		}
	}
	return items, lastScanned, nil
}

// adoListCandidateMatches applies ListWorkItems' client-side filters: the
// common open/closed state, the label and field predicates, and the exact
// label recheck behind WIQL's substring CONTAINS.
func adoListCandidateMatches(req ListWorkItemsRequest, requestedState string, item WorkItem) (bool, error) {
	if (requestedState == "open" || requestedState == "closed") && item.State != requestedState {
		return false, nil
	}
	matched, err := req.MatchesLabelPredicate(item.Labels)
	if err != nil || !matched {
		return false, err
	}
	matched, err = req.MatchesFieldPredicate(item.Fields)
	if err != nil || !matched {
		return false, err
	}
	return hasAllLabels(item.Labels, req.Labels), nil
}

// adoRetryableRequest reports whether send may resend a request after a
// transient failure: an idempotent method, or a POST to workitemsbatch, which
// only reads.
func adoRetryableRequest(method, endpoint string) bool {
	if isIdempotentHTTPMethod(method) {
		return true
	}
	if method != http.MethodPost {
		return false
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	return strings.HasSuffix(parsed.Path, adoWorkItemsBatchPath)
}
