package apicontract

import "github.com/goobers/goobers/internal/workbench"

func workbenchEnum(values ...string) map[string]any {
	return map[string]any{"type": "string", "enum": values}
}
func workbenchArray(name string, limit int) map[string]any {
	return map[string]any{"type": "array", "maxItems": limit, "items": schemaRef(name)}
}
func openAPIWorkbenchDocumentSchemas() map[string]any {
	return map[string]any{
		"WorkbenchRepository":       closedChildObject([]string{"provider", "owner", "name"}, map[string]any{"provider": workbenchEnum("github", "ado"), "owner": sessionString(256), "name": sessionString(256), "project": sessionString(256)}),
		"WorkbenchEdge":             closedChildObject([]string{"edgeId", "kind", "from", "to"}, map[string]any{"edgeId": sessionString(41), "kind": workbenchEnum("parent-of", "blocked-by", "contributes-to", "references", "milestone-member", "implemented-by"), "from": schemaRef("WorkbenchNodeRef"), "to": schemaRef("WorkbenchNodeRef"), "rationale": sessionString(4096)}),
		"WorkbenchObjective":        closedChildObject([]string{"schemaVersion", "objectiveId", "title"}, map[string]any{"schemaVersion": workbenchEnum("objectives/v1"), "objectiveId": sessionString(40), "title": sessionString(workbench.MaxSourceBytes), "edges": workbenchArray("WorkbenchEdge", workbench.MaxSourceEdges)}),
		"WorkbenchAlias":            closedChildObject([]string{"name", "target"}, map[string]any{"name": sessionString(64), "target": schemaRef("WorkbenchNodeRef")}),
		"WorkbenchManifest":         closedChildObject([]string{"schemaVersion", "edges"}, map[string]any{"schemaVersion": workbenchEnum("relationships/v1"), "aliases": workbenchArray("WorkbenchAlias", 128), "edges": workbenchArray("WorkbenchEdge", workbench.MaxSourceEdges)}),
		"WorkbenchSourceProvenance": closedChildObject([]string{"commit", "blobId", "contentDigest"}, map[string]any{"commit": sessionString(64), "blobId": sessionString(64), "contentDigest": sessionString(64), "etag": sessionString(4096)}),
		"WorkbenchDocumentFile":     closedChildObject([]string{"path", "status"}, map[string]any{"path": sessionString(1024), "status": workbenchEnum("available", "unavailable", "invalid-source", "oversized"), "provenance": schemaRef("WorkbenchSourceProvenance"), "ref": schemaRef("WorkbenchNodeRef"), "body": sessionString(workbench.MaxSourceBytes), "objective": schemaRef("WorkbenchObjective"), "manifest": schemaRef("WorkbenchManifest")}),
		"WorkbenchDocumentPage": closedChildObject([]string{"sourceBindingId", "repository", "branch", "commit", "sourceTargetDigest", "files", "startOffset", "totalPaths", "exhausted", "coverage"}, map[string]any{
			"sourceBindingId": sessionString(64), "repository": schemaRef("WorkbenchRepository"), "branch": sessionString(1024), "commit": sessionString(64), "sourceTargetDigest": sessionString(64), "files": workbenchArray("WorkbenchDocumentFile", workbench.MaxDocumentPageFiles), "startOffset": workbenchNonnegative(), "totalPaths": map[string]any{"type": "integer", "minimum": 1, "maximum": 128}, "nextCursor": sessionString(workbench.MaxBacklogCursorBytes), "exhausted": map[string]any{"type": "boolean"}, "coverage": workbenchEnum("complete", "partial"), "reasons": workbenchStrings(128, 128),
		}),
	}
}
