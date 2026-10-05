package workbenchprovider

import (
	"net/url"
	"strings"

	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/providers"
)

func (r *BacklogReader) relationships(item providers.WorkItem) ([]workbench.NativeRelationship, workbench.RelationshipCoverage) {
	if r.repository.Provider == providers.ProviderGitHub {
		return r.githubRelationships(item)
	}
	coverage := workbench.RelationshipCoverage{Parents: "complete", Blockers: "complete", Milestones: "unsupported"}
	var result []workbench.NativeRelationship
	for _, link := range item.Links {
		kind, incoming := "", false
		switch link.Rel {
		case "System.LinkTypes.Hierarchy-Reverse":
			kind, incoming = "parent-of", true
		case "System.LinkTypes.Dependency-Reverse":
			kind = "blocked-by"
		default:
			continue
		}
		id := r.adoLinkedID(link.URL)
		if id == "" {
			if incoming {
				coverage.Parents = "partial"
			} else {
				coverage.Blockers = "partial"
			}
			continue
		}
		// ADO IDs are organization-scoped, while a binding is project-scoped.
		// Inline relations do not prove the target project's visibility.
		result = append(result, workbench.NativeRelationship{Kind: kind, Incoming: incoming, Target: workbench.NativeTarget{Kind: "work-item", StableID: id, Locator: workbench.SourceLocator{ID: id}}})
	}
	return result, coverage
}

func (r *BacklogReader) githubRelationships(item providers.WorkItem) ([]workbench.NativeRelationship, workbench.RelationshipCoverage) {
	coverage := workbench.RelationshipCoverage{Parents: "not-loaded", Blockers: "not-loaded", Milestones: "complete"}
	// The legacy provider Parent slot contains a milestone, not issue ancestry.
	milestone := item.Parent
	if milestone == nil {
		return nil, coverage
	}
	if milestone.Type != "milestone" || milestone.Provider != providers.ProviderGitHub || !positiveID(milestone.StableID) || !positiveID(milestone.ID) {
		coverage.Milestones = "partial"
		return nil, coverage
	}
	ref := r.ref("milestone", milestone.StableID)
	return []workbench.NativeRelationship{{Kind: "milestone-member", Target: workbench.NativeTarget{Ref: &ref, Kind: "milestone", StableID: milestone.StableID, Locator: workbench.SourceLocator{ID: milestone.ID}}}}, coverage
}

func (r *BacklogReader) adoLinkedID(raw string) string {
	if len(raw) > 2048 {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "dev.azure.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 5 && len(parts) != 6 {
		return ""
	}
	if !strings.EqualFold(parts[0], r.repository.Owner) {
		return ""
	}
	tail := parts[len(parts)-4:]
	if !strings.EqualFold(strings.Join(tail[:3], "/"), "_apis/wit/workitems") || !positiveID(tail[3]) {
		return ""
	}
	return tail[3]
}
