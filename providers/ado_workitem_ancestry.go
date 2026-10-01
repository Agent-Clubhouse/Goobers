package providers

import (
	"context"
	"slices"
	"strconv"

	apiintegrity "github.com/goobers/goobers/api/integrity"
)

// adoAncestryDefaultFields are the Azure Boards fields that carry a parent's
// intent when a walk names none: its description and acceptance criteria.
var adoAncestryDefaultFields = []string{"System.Description", "Microsoft.VSTS.Common.AcceptanceCriteria"}

// AncestryRoot starts an ancestry walk at a directly referenced work item. Its
// parent is the System.LinkTypes.Hierarchy-Reverse relation mapADOWorkItem
// already preserved.
func (p *ADOProvider) AncestryRoot(repo RepositoryRef, item WorkItem) WorkItemNode {
	node := WorkItemNode{Provider: ProviderADO, Project: p.project(repo), ID: item.ID, Type: item.Type, Title: item.Title, URL: item.URL, Integrity: item.Integrity}
	if project, ok := item.Fields["System.TeamProject"].(string); ok && project != "" {
		node.Project = project
	}
	if item.Parent != nil && item.Parent.Type == "parent" {
		node.ParentID = item.Parent.ID
	}
	return node
}

// ReadWorkItemParents reads every child's parent in one workitemsbatch call
// (with relations, so each parent carries its own parent hint). The batch's
// Omit error policy returns nothing for an id that is deleted or unreadable,
// which is reported as not-found: the provider does not say which.
func (p *ADOProvider) ReadWorkItemParents(ctx context.Context, repo RepositoryRef, children []WorkItemNode, fields []string) ([]WorkItemParentRead, error) {
	reads := make([]WorkItemParentRead, len(children))
	ids := make([]int, 0, len(children))
	for i, child := range children {
		reads[i].ParentID = child.ParentID
		id, err := strconv.Atoi(child.ParentID)
		if err != nil || id <= 0 {
			reads[i].Omission, reads[i].Detail = AncestryOmitReadFailed, "invalid parent id"
			continue
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	items, err := p.getWorkItemsBatch(ctx, repo, ids)
	if err != nil {
		if ctxErr := ancestryContextError(ctx, err); ctxErr != nil {
			return nil, ctxErr
		}
		return failAncestryReads(reads, err), nil
	}
	byID := make(map[string]adoWorkItem, len(items))
	for _, item := range items {
		byID[strconv.Itoa(item.ID)] = item
	}
	for i := range reads {
		if reads[i].Omission != "" {
			continue
		}
		item, ok := byID[reads[i].ParentID]
		if !ok {
			reads[i].Omission = AncestryOmitNotFound
			continue
		}
		node := p.adoAncestryNode(repo, item, fields)
		reads[i].Parent = &node
	}
	return reads, nil
}

// adoAncestryNode maps a raw work item to an ancestry node. State is the
// native System.State: a custom process type's state needs no category
// lookup, so any work-item type maps without an extra read.
func (p *ADOProvider) adoAncestryNode(repo RepositoryRef, item adoWorkItem, fields []string) WorkItemNode {
	project := stringField(item.Fields, "System.TeamProject")
	if project == "" {
		project = p.project(repo)
	}
	parent, _, _ := adoHierarchy(item.Relations)
	node := WorkItemNode{
		Provider:  ProviderADO,
		Project:   project,
		ID:        strconv.Itoa(item.ID),
		Type:      stringField(item.Fields, "System.WorkItemType"),
		Title:     stringField(item.Fields, "System.Title"),
		State:     stringField(item.Fields, "System.State"),
		URL:       item.URL,
		Fields:    selectAncestryFields(fields, adoAncestryDefaultFields, func(name string) string { return stringField(item.Fields, name) }),
		Integrity: apiintegrity.Unapproved,
	}
	if parent != nil {
		node.ParentID = parent.ID
	}
	return node
}

// failAncestryReads marks every still-pending read with err's omission.
func failAncestryReads(reads []WorkItemParentRead, err error) []WorkItemParentRead {
	reason, detail := AncestryReadOmission(err)
	for i := range reads {
		if reads[i].Omission == "" {
			reads[i].Omission, reads[i].Detail = reason, detail
		}
	}
	return reads
}
