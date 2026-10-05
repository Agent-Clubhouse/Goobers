package apicontract

import "github.com/goobers/goobers/internal/workbench"

func metadataCommitSchema() map[string]any {
	return map[string]any{"type": "string", "pattern": "^[0-9a-f]{40}$"}
}
func openAPIWorkbenchProposalSchemas() map[string]any {
	input := closedChildObject([]string{"path", "expected"}, map[string]any{"path": sessionString(1024), "expected": schemaRef("MetadataRevision"), "field": workbenchEnum("title", "description"), "value": sessionString(workbench.MaxSourceBytes), "relationship": schemaRef("MetadataRelationshipEdit"), "objective": schemaRef("MetadataObjectiveAssignment"), "alias": schemaRef("MetadataAliasEdit")})
	input["oneOf"] = []map[string]any{
		metadataOperationVariant([]string{"field", "value"}, "relationship", "objective", "alias"),
		metadataOperationVariant([]string{"relationship"}, "field", "value", "objective", "alias"),
		metadataOperationVariant([]string{"objective"}, "field", "value", "relationship", "alias"),
		metadataOperationVariant([]string{"alias"}, "field", "value", "relationship", "objective"),
	}
	edge := closedChildObject([]string{"edgeId", "kind", "from", "to"}, map[string]any{"edgeId": map[string]any{"type": "string", "pattern": "^edge-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"}, "kind": workbenchEnum("references", "contributes-to"), "from": schemaRef("WorkbenchNodeRef"), "to": schemaRef("WorkbenchNodeRef"), "rationale": sessionString(4096)})
	return map[string]any{
		"MetadataChangeRequest":       input,
		"MetadataObjectiveAssignment": closedChildObject([]string{"objectiveId", "title"}, map[string]any{"objectiveId": map[string]any{"type": "string", "pattern": "^obj-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"}, "title": sessionString(512)}),
		"MetadataAliasEdit":           closedChildObject([]string{"action", "alias"}, map[string]any{"action": workbenchEnum("add", "remove"), "alias": closedChildObject([]string{"name", "target"}, map[string]any{"name": map[string]any{"type": "string", "pattern": "^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$", "maxLength": 64}, "target": schemaRef("WorkbenchNodeRef")})}),
		"MetadataRevision":            closedChildObject([]string{"commit", "blobId", "contentDigest"}, map[string]any{"commit": metadataCommitSchema(), "blobId": metadataCommitSchema(), "contentDigest": workbenchDigest()}),
		"MetadataRelationshipEdit":    closedChildObject([]string{"action", "edge"}, map[string]any{"action": workbenchEnum("add", "remove"), "edge": edge}),
		"MetadataPreview":             closedChildObject([]string{"path", "expected", "targetDigest", "operationDigest", "proposedContentDigest", "changed", "before", "after"}, map[string]any{"path": sessionString(1024), "expected": schemaRef("MetadataRevision"), "targetDigest": workbenchDigest(), "operationDigest": workbenchDigest(), "proposedContentDigest": workbenchDigest(), "changed": map[string]any{"type": "boolean"}, "before": sessionString(workbench.MaxSourceBytes), "after": sessionString(workbench.MaxSourceBytes)}),
		"MetadataProposalPR":          closedChildObject([]string{"id", "number", "url"}, map[string]any{"id": map[string]any{"type": "string", "pattern": "^[1-9][0-9]*$", "maxLength": 19}, "number": map[string]any{"type": "integer", "minimum": 1}, "url": sessionString(2048)}),
		"MetadataProposalPhase":       closedChildObject([]string{"name", "outcome", "claimedAt"}, map[string]any{"name": workbenchEnum("tree", "commit", "branch", "pull-request"), "outcome": workbenchEnum("pending", "acknowledged", "unknown", "not-attempted"), "claimedAt": dateTimeSchema(), "finishedAt": dateTimeSchema(), "treeId": metadataCommitSchema(), "commitId": metadataCommitSchema(), "pullRequest": schemaRef("MetadataProposalPR")}),
		"MetadataProposalObservation": closedChildObject([]string{"phase", "at", "found", "matches"}, map[string]any{"phase": map[string]any{"type": "integer", "minimum": 0, "maximum": 3}, "at": dateTimeSchema(), "found": map[string]any{"type": "boolean"}, "matches": map[string]any{"type": "boolean"}, "treeId": metadataCommitSchema(), "commitId": metadataCommitSchema(), "pullRequest": schemaRef("MetadataProposalPR")}),
		"MetadataProposalCommand":     metadataCommandSchema(),
	}
}
func metadataCommandSchema() map[string]any {
	return closedChildObject([]string{"id", "gaggle", "sourceBindingId", "actor", "path", "state", "duplicate", "requestDigest", "operationDigest", "expected", "acceptedAt", "phases", "observations", "omittedObservations", "nextAction"}, map[string]any{
		"id": map[string]any{"type": "string", "pattern": "^workbench-[0-9a-f]{32}$"}, "gaggle": sessionString(128), "sourceBindingId": sessionString(64), "actor": schemaRef("WorkbenchCommandActor"), "path": sessionString(1024), "state": workbenchEnum("accepted", "prepared", "attempting", "unknown", "blocked", "confirmed", "observed", "not-applied"), "duplicate": map[string]any{"type": "boolean"}, "requestDigest": workbenchDigest(), "operationDigest": workbenchDigest(), "expected": schemaRef("MetadataRevision"), "proposedContentDigest": workbenchDigest(), "branch": sessionString(128), "acceptedAt": dateTimeSchema(), "completedAt": dateTimeSchema(), "phases": workbenchArray("MetadataProposalPhase", 4), "observations": workbenchArray("MetadataProposalObservation", 16), "omittedObservations": map[string]any{"type": "integer", "minimum": 0}, "nextAction": sessionString(1024),
	})
}
func workbenchProposalResponses(id RouteID) map[string]any {
	name := "MetadataProposalCommand"
	if id == RouteWorkbenchProposalPreview {
		name = "MetadataPreview"
	}
	return map[string]any{"200": jsonResponse("Current authorized proposal evidence; observations never advance source writes", schemaRef(name)), "default": jsonResponse("Structured API error", schemaRef("ErrorEnvelope"))}
}

func workbenchProposalRequestBody(id RouteID) map[string]any {
	schema := closedChildObject(nil, map[string]any{})
	if id == RouteWorkbenchProposalPreview || id == RouteWorkbenchProposalSubmit {
		schema = schemaRef("MetadataChangeRequest")
	}
	return map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": schema}}}
}

func metadataOperationVariant(required []string, excluded ...string) map[string]any {
	choices := make([]map[string]any, 0, len(excluded))
	for _, key := range excluded {
		choices = append(choices, map[string]any{"required": []string{key}})
	}
	return map[string]any{"required": required, "not": map[string]any{"anyOf": choices}}
}
