package workbenchprovider

import (
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/providers"
)

func (r *BacklogReader) project(item providers.WorkItem) (workbench.BacklogItem, int, error) {
	if item.Provider != r.repository.Provider || !positiveID(item.ID) || !positiveID(item.StableID) || !utf8.ValidString(item.Title) || len(item.Revision) > 256 {
		return workbench.BacklogItem{}, 0, ErrInvalidItem
	}
	if len(item.Labels) > 128 || len(item.NativeAssignees) > 128 || len(item.Links) > 256 {
		return workbench.BacklogItem{}, 0, ErrItemTooLarge
	}
	result := workbench.BacklogItem{Ref: r.ref("work-item", item.StableID), Locator: workbench.SourceLocator{ID: item.ID}, Revision: item.Revision, Type: item.Type, Title: item.Title,
		Description: item.Body, AcceptanceCriteria: item.AcceptanceCriteria, State: item.State, Labels: append([]string(nil), item.Labels...), Assignees: append([]string(nil), item.NativeAssignees...), UpdatedAt: item.UpdatedAt}
	if err := r.nativeFields(item, &result); err != nil {
		return workbench.BacklogItem{}, 0, err
	}
	result.Objective = slices.Contains(r.objectives.IDs, item.StableID) || slices.Contains(r.objectives.Types, item.Type)
	for _, label := range result.Labels {
		result.Objective = result.Objective || slices.Contains(r.objectives.Labels, label)
	}
	result.Relationships, result.RelationshipCoverage = r.relationships(item)
	raw, err := json.Marshal(result)
	if err != nil {
		return workbench.BacklogItem{}, 0, ErrInvalidItem
	}
	if len(raw) > workbench.MaxBacklogItemBytes {
		return workbench.BacklogItem{}, 0, ErrItemTooLarge
	}
	return result, len(raw), nil
}

func (r *BacklogReader) nativeFields(item providers.WorkItem, result *workbench.BacklogItem) error {
	switch r.repository.Provider {
	case providers.ProviderGitHub:
		if !r.githubItemURL(item.URL, item.ID) {
			return ErrInvalidItem
		}
		result.Locator.URL = item.URL
		result.RevisionSemantics = "timestamp-preflight"
	case providers.ProviderADO:
		project, _ := item.Fields["System.TeamProject"].(string)
		if !strings.EqualFold(project, r.repository.Project) || item.StableID != item.ID || r.adoLinkedID(item.URL) != item.ID {
			return ErrInvalidItem
		}
		state, ok := item.Fields["System.State"].(string)
		if !ok {
			return ErrInvalidItem
		}
		result.State = state
		result.Description = item.Description
		result.RevisionSemantics = "atomic-revision-test"
		result.Locator.URL = "https://dev.azure.com/" + url.PathEscape(r.repository.Owner) + "/" + url.PathEscape(r.repository.Project) + "/_workitems/edit/" + item.ID
		// Preserve all native tags, including source-owned legacy tags hidden
		// from the scheduler's processing-label projection.
		result.Labels = nil
		if tags, ok := item.Fields["System.Tags"].(string); ok {
			for _, tag := range strings.Split(tags, ";") {
				if label := strings.TrimSpace(tag); label != "" {
					result.Labels = append(result.Labels, label)
				}
			}
		}
		if len(result.Labels) > 128 {
			return ErrItemTooLarge
		}
	}
	return nil
}

func (r *BacklogReader) ref(kind, id string) workbench.NodeRef {
	return workbench.NodeRef{GaggleID: r.gaggle, SourceBindingID: r.binding, Kind: kind, SourceID: id}
}

func component(value string) bool {
	return value != "" && value != "." && value != ".." && len(value) <= 253 && strings.TrimSpace(value) == value && utf8.ValidString(value) && !strings.ContainsAny(value, "/\\?#%:") && strings.IndexFunc(value, unicode.IsControl) < 0
}

func (r *BacklogReader) githubItemURL(raw, id string) bool {
	u, err := url.Parse(raw)
	return err == nil && len(raw) <= 2048 && u.Scheme == "https" && u.Host == "github.com" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && strings.EqualFold(u.Path, "/"+r.repository.Owner+"/"+r.repository.Name+"/issues/"+id)
}
