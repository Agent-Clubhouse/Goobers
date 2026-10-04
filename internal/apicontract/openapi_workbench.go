package apicontract

import "github.com/goobers/goobers/internal/workbench"

func workbenchStrings(maxItems, maxLength int) map[string]any {
	return map[string]any{"type": "array", "maxItems": maxItems, "items": sessionString(maxLength)}
}
func workbenchNonnegative() map[string]any { return map[string]any{"type": "integer", "minimum": 0} }
func workbenchCoverage() map[string]any {
	return map[string]any{"type": "string", "enum": []string{"complete", "partial", "not-loaded", "unsupported"}}
}

func openAPIWorkbenchSchemas() map[string]any {
	return map[string]any{
		"WorkbenchNodeRef": closedChildObject([]string{"gaggleId", "sourceBindingId", "kind", "sourceId"}, map[string]any{
			"gaggleId": sessionString(253), "sourceBindingId": sessionString(64), "kind": map[string]any{"type": "string", "enum": []string{"work-item", "milestone", "pull-request", "document", "objective-document"}}, "sourceId": sessionString(512),
		}),
		"SourceLocator":        closedChildObject([]string{"id"}, map[string]any{"id": sessionString(512), "url": sessionString(4096)}),
		"NativeTarget":         closedChildObject([]string{"kind", "locator"}, map[string]any{"ref": schemaRef("WorkbenchNodeRef"), "kind": sessionString(64), "stableId": sessionString(512), "locator": schemaRef("SourceLocator")}),
		"NativeRelationship":   closedChildObject([]string{"kind", "target"}, map[string]any{"kind": sessionString(64), "incoming": map[string]any{"type": "boolean"}, "target": schemaRef("NativeTarget")}),
		"RelationshipCoverage": closedChildObject([]string{"parents", "blockers", "milestones"}, map[string]any{"parents": workbenchCoverage(), "blockers": workbenchCoverage(), "milestones": workbenchCoverage()}),
		"BacklogItemRequest":   closedChildObject([]string{"id"}, map[string]any{"id": sessionString(512), "expectedSourceId": sessionString(512)}),
		"BacklogPageRequest":   closedChildObject([]string{}, map[string]any{"cursor": sessionString(workbench.MaxBacklogCursorBytes), "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": workbench.MaxBacklogPageItems}}),
		"BacklogItem": closedChildObject([]string{"ref", "locator", "revisionSemantics", "type", "title", "state", "objective", "relationshipCoverage"}, map[string]any{
			"ref": schemaRef("WorkbenchNodeRef"), "locator": schemaRef("SourceLocator"), "revision": sessionString(512), "revisionSemantics": sessionString(64), "type": sessionString(workbench.MaxBacklogItemBytes), "title": sessionString(workbench.MaxBacklogItemBytes), "description": sessionString(workbench.MaxBacklogItemBytes), "acceptanceCriteria": sessionString(workbench.MaxBacklogItemBytes), "state": sessionString(workbench.MaxBacklogItemBytes), "labels": workbenchStrings(128, workbench.MaxBacklogItemBytes), "assignees": workbenchStrings(128, workbench.MaxBacklogItemBytes), "objective": map[string]any{"type": "boolean"}, "updatedAt": dateTimeSchema(), "relationships": map[string]any{"type": "array", "maxItems": 2000, "items": schemaRef("NativeRelationship")}, "relationshipCoverage": schemaRef("RelationshipCoverage"),
		}),
		"BacklogPage": closedChildObject([]string{"items", "exhausted", "partial", "candidates", "omitted", "sourceTargetDigest"}, map[string]any{
			"items": map[string]any{"type": "array", "maxItems": workbench.MaxBacklogPageItems, "items": schemaRef("BacklogItem")}, "nextCursor": sessionString(workbench.MaxBacklogCursorBytes), "exhausted": map[string]any{"type": "boolean"}, "partial": map[string]any{"type": "boolean"}, "reasons": workbenchStrings(32, 128), "candidates": workbenchNonnegative(), "omitted": workbenchNonnegative(), "sourceTargetDigest": map[string]any{"type": "string", "pattern": "^[0-9a-f]{64}$"},
		}),
	}
}
