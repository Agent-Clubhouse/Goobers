package workbenchprovider

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/providers"
)

func (r *BacklogReader) expandRelationships(ctx context.Context, item providers.WorkItem, result *workbench.BacklogItem) error {
	client, ok := r.client.(providers.WorkItemRelationshipReader)
	if !ok {
		return nil
	}
	relationships, err := client.ReadWorkItemRelationships(ctx, r.repository, item)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if len(relationships.Parents)+len(relationships.Blockers) > providers.MaxSelectedRelationshipTargets {
		return ErrItemTooLarge
	}
	kept := result.Relationships[:0]
	for _, edge := range result.Relationships {
		if edge.Kind != "parent-of" && edge.Kind != "blocked-by" {
			kept = append(kept, edge)
		}
	}
	result.Relationships = kept
	parents, complete := r.selectedRelationships(relationships.Parents, "parent-of", true)
	result.Relationships = append(result.Relationships, parents...)
	result.RelationshipCoverage.Parents = relationshipCoverage(complete && relationships.ParentsComplete)
	blockers, complete := r.selectedRelationships(relationships.Blockers, "blocked-by", false)
	result.Relationships = append(result.Relationships, blockers...)
	result.RelationshipCoverage.Blockers = relationshipCoverage(complete && relationships.BlockersComplete)
	raw, err := json.Marshal(result)
	if err != nil {
		return ErrInvalidItem
	}
	if len(raw) > workbench.MaxBacklogItemBytes {
		return ErrItemTooLarge
	}
	return nil
}
func relationshipCoverage(complete bool) string {
	if complete {
		return "complete"
	}
	return "partial"
}
func (r *BacklogReader) selectedRelationships(targets []providers.WorkItemRelationTarget, kind string, incoming bool) ([]workbench.NativeRelationship, bool) {
	result := make([]workbench.NativeRelationship, 0, len(targets))
	complete := true
	seen := map[string]bool{}
	for _, target := range targets {
		if len(target.ID) > 256 || len(target.StableID) > 256 || len(target.URL) > 2048 {
			return nil, false
		}
		node := workbench.NativeTarget{Kind: "work-item"}
		if positiveID(target.ID) {
			node.Locator.ID = target.ID
		}
		if positiveID(target.StableID) {
			node.StableID = target.StableID
		}
		verified := target.Verified && positiveID(target.ID) && positiveID(target.StableID) && r.verifiedRelationURL(target)
		if verified && !seen[target.StableID] {
			ref := r.ref("work-item", target.StableID)
			node.Ref = &ref
			node.Locator.URL = target.URL
			seen[target.StableID] = true
		} else {
			complete = false
		}
		result = append(result, workbench.NativeRelationship{Kind: kind, Incoming: incoming, Target: node})
	}
	return result, complete
}
func (r *BacklogReader) verifiedRelationURL(target providers.WorkItemRelationTarget) bool {
	if r.repository.Provider == providers.ProviderGitHub {
		return r.githubItemURL(target.URL, target.ID)
	}
	if target.ID != target.StableID {
		return false
	}
	expected := "https://dev.azure.com/" + url.PathEscape(r.repository.Owner) + "/" + url.PathEscape(r.repository.Project) + "/_workitems/edit/" + target.ID
	return strings.EqualFold(target.URL, expected)
}
