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
		"WorkbenchSourceView": closedChildObject([]string{"bindingId", "kind", "provider", "owner"}, map[string]any{
			"bindingId": sessionString(64), "kind": map[string]any{"type": "string", "enum": []string{"backlog", "documents", "relationships"}}, "provider": map[string]any{"type": "string", "enum": []string{"github", "ado"}}, "owner": sessionString(256), "project": sessionString(256), "repository": sessionString(256), "branch": sessionString(1024), "paths": workbenchStrings(128, 1024), "writeFields": workbenchStrings(5, 32), "writeRelationships": workbenchStrings(6, 32),
		}),
		"WorkbenchSourcePage": closedChildObject([]string{"items", "generation"}, map[string]any{"items": map[string]any{"type": "array", "maxItems": 32, "items": schemaRef("WorkbenchSourceView")}, "generation": map[string]any{"type": "string", "pattern": "^[0-9a-f]{64}$"}}),
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

func workbenchReadParameters(id RouteID) []map[string]any {
	switch id {
	case RouteWorkbenchItems:
		return []map[string]any{{"name": "cursor", "in": "query", "schema": sessionString(workbench.MaxBacklogCursorBytes)}, {"name": "limit", "in": "query", "schema": map[string]any{"type": "integer", "minimum": 1, "maximum": workbench.MaxBacklogPageItems, "default": 50}}}
	case RouteWorkbenchItem:
		return []map[string]any{{"name": "expectedSourceId", "in": "query", "schema": sessionString(512)}}
	default:
		return nil
	}
}
func workbenchReadResponses(id RouteID) map[string]any {
	name := "WorkbenchSourcePage"
	switch id {
	case RouteWorkbenchItems:
		name = "BacklogPage"
	case RouteWorkbenchItem:
		name = "BacklogItem"
	}
	return map[string]any{"200": jsonResponse("Current authorized source projection; coverage may be partial", schemaRef(name)), "default": jsonResponse("Structured API error", schemaRef("ErrorEnvelope"))}
}
