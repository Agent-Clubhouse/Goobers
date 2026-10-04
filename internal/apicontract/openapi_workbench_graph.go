package apicontract

import (
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/internal/workbenchgraph"
)

func openAPIWorkbenchGraphSchemas() map[string]any {
	boolean := map[string]any{"type": "boolean"}
	digest := map[string]any{"type": "string", "pattern": "^[0-9a-f]{64}$"}
	return map[string]any{
		"WorkbenchGraph": closedChildObject([]string{"generation", "gaggleId", "nodes", "edges", "documents", "aliases", "sources", "conflicts", "partial"}, map[string]any{
			"generation": digest, "gaggleId": sessionString(253), "nodes": workbenchArray("WorkbenchGraphNode", workbenchgraph.MaxNodes), "edges": workbenchArray("WorkbenchGraphEdge", workbenchgraph.MaxEdges), "documents": workbenchArray("WorkbenchGraphDocument", 4096), "aliases": workbenchArray("WorkbenchGraphAlias", 4096), "sources": workbenchArray("WorkbenchGraphCoverage", 32), "conflicts": workbenchArray("WorkbenchGraphConflict", 110000), "partial": boolean,
		}),
		"WorkbenchGraphNode":        closedChildObject([]string{"key", "observations", "conflict"}, map[string]any{"key": digest, "observations": workbenchArray("WorkbenchGraphObservation", 51200), "conflict": boolean}),
		"WorkbenchGraphObservation": closedChildObject([]string{"contentDigest", "ref", "title", "revision", "locator", "objective"}, map[string]any{"contentDigest": digest, "ref": schemaRef("WorkbenchNodeRef"), "title": sessionString(workbench.MaxBacklogItemBytes), "type": sessionString(workbench.MaxBacklogItemBytes), "state": sessionString(workbench.MaxBacklogItemBytes), "revision": sessionString(512), "locator": schemaRef("SourceLocator"), "objective": boolean, "path": sessionString(1024), "provenance": schemaRef("WorkbenchSourceProvenance")}),
		"WorkbenchGraphEndpoint":    closedChildObject([]string{"resolved"}, map[string]any{"ref": schemaRef("WorkbenchNodeRef"), "native": schemaRef("NativeTarget"), "resolved": boolean}),
		"WorkbenchGraphOwner":       closedChildObject([]string{"kind", "sourceBindingId"}, map[string]any{"kind": workbenchEnum("native", "frontmatter", "manifest"), "sourceBindingId": sessionString(64), "path": sessionString(1024), "field": sessionString(256), "object": schemaRef("WorkbenchNodeRef")}),
		"WorkbenchGraphEdge":        closedChildObject([]string{"key", "kind", "from", "to", "origin", "owner", "conflict"}, map[string]any{"key": digest, "edgeId": sessionString(41), "kind": workbenchEnum("parent-of", "blocked-by", "contributes-to", "references", "milestone-member", "implemented-by"), "from": schemaRef("WorkbenchGraphEndpoint"), "to": schemaRef("WorkbenchGraphEndpoint"), "rationale": sessionString(4096), "origin": workbenchEnum("native", "authored"), "owner": schemaRef("WorkbenchGraphOwner"), "conflict": boolean}),
		"WorkbenchGraphDocument":    closedChildObject([]string{"sourceBindingId", "path", "status"}, map[string]any{"sourceBindingId": sessionString(64), "path": sessionString(1024), "status": workbenchEnum("available", "unavailable", "invalid-source", "oversized"), "provenance": schemaRef("WorkbenchSourceProvenance"), "ref": schemaRef("WorkbenchNodeRef")}),
		"WorkbenchGraphAlias":       closedChildObject([]string{"name", "target", "owner"}, map[string]any{"name": sessionString(64), "target": schemaRef("WorkbenchGraphEndpoint"), "owner": schemaRef("WorkbenchGraphOwner")}),
		"WorkbenchGraphCoverage":    closedChildObject([]string{"sourceBindingId", "kind", "status", "consistency"}, map[string]any{"sourceBindingId": sessionString(64), "kind": workbenchEnum("backlog", "documents", "relationships"), "status": workbenchEnum("complete", "partial", "not-read"), "consistency": workbenchEnum("commit-pinned", "native-non-snapshot"), "sourceTargetDigest": digest, "commit": sessionString(40), "reasons": workbenchStrings(128, 128)}),
		"WorkbenchGraphConflict":    closedChildObject([]string{"kind", "keys"}, map[string]any{"kind": workbenchEnum("edge-id-conflict", "edge-owner-conflict", "native-observation-conflict", "objective-location-conflict"), "keys": workbenchStrings(workbenchgraph.MaxEdges, 64)}),
	}
}
