package providers

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ReadWorkItemRelationships hydrates at most 64 explicit same-org candidates in
// one batch. TeamProject proves source membership; links alone do not. It never
// maps workflow states or recursively reads targets' relationships.
func (p *ADOProvider) ReadWorkItemRelationships(ctx context.Context, repo RepositoryRef, item WorkItem) (WorkItemRelationships, error) {
	result := WorkItemRelationships{ParentsComplete: true, BlockersComplete: true}
	if item.Provider != ProviderADO || !nativePositiveID(item.ID) || !strings.EqualFold(stringField(item.Fields, "System.TeamProject"), p.project(repo)) || len(item.Links) > 256 {
		return result, ErrAttentionChanged
	}
	ids := make([]int, 0, 64)
	seen := map[int]bool{}
	for _, link := range item.Links {
		parent := link.Rel == "System.LinkTypes.Hierarchy-Reverse"
		if !parent && link.Rel != "System.LinkTypes.Dependency-Reverse" {
			continue
		}
		target := WorkItemRelationTarget{ID: scopedADORelationID(repo, link.URL)}
		if target.ID != "" {
			target.StableID = target.ID
		}
		count := len(result.Parents) + len(result.Blockers)
		if count >= 64 {
			if parent {
				result.ParentsComplete = false
			} else {
				result.BlockersComplete = false
			}
			continue
		}
		if parent {
			result.Parents = append(result.Parents, target)
		} else {
			result.Blockers = append(result.Blockers, target)
		}
		id, _ := strconv.Atoi(target.ID)
		if id > 0 && !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	raw, err := p.selectedRelationBatch(ctx, repo, ids)
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		result.ParentsComplete = false
		result.BlockersComplete = false
		return result, nil
	}
	verified := map[string]bool{}
	for _, target := range raw {
		id := strconv.Itoa(target.ID)
		verified[id] = seen[target.ID] && strings.EqualFold(stringField(target.Fields, "System.TeamProject"), p.project(repo)) && scopedADORelationID(repo, target.URL) == id
	}
	result.ParentsComplete = qualifyADOTargets(repo, result.Parents, verified) && result.ParentsComplete
	result.BlockersComplete = qualifyADOTargets(repo, result.Blockers, verified) && result.BlockersComplete
	return result, nil
}
func qualifyADOTargets(repo RepositoryRef, targets []WorkItemRelationTarget, verified map[string]bool) bool {
	complete := true
	for i := range targets {
		target := &targets[i]
		target.Verified = verified[target.ID]
		if !target.Verified {
			complete = false
			continue
		}
		target.URL = "https://dev.azure.com/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Project) + "/_workitems/edit/" + target.ID
	}
	return complete
}
func scopedADORelationID(repo RepositoryRef, raw string) string {
	if len(raw) > 2048 {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "dev.azure.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if (len(parts) != 5 && len(parts) != 6) || !strings.EqualFold(parts[0], repo.Owner) {
		return ""
	}
	tail := parts[len(parts)-4:]
	if !strings.EqualFold(strings.Join(tail[:3], "/"), "_apis/wit/workitems") || !nativePositiveID(tail[3]) {
		return ""
	}
	return tail[3]
}

// Keep ambiguous duplicate IDs unresolved instead of using last-response-wins.
func (p *ADOProvider) selectedRelationBatch(ctx context.Context, repo RepositoryRef, ids []int) ([]adoWorkItem, error) {
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
	var response adoWorkItemsBatchResponse
	if err = p.do(ctx, http.MethodPost, endpoint, adoWorkItemsBatchRequest{IDs: ids, Expand: "Relations", ErrorPolicy: "Omit"}, &response); err != nil {
		return nil, err
	}
	if len(response.Value) > 64 {
		return nil, ErrAttentionChanged
	}
	counts := map[int]int{}
	for _, item := range response.Value {
		if item != nil {
			counts[item.ID]++
		}
	}
	result := make([]adoWorkItem, 0, len(response.Value))
	for _, item := range response.Value {
		if item != nil && counts[item.ID] == 1 {
			result = append(result, *item)
		}
	}
	return result, nil
}
