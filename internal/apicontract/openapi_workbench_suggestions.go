package apicontract

func suggestionSequence(minimum int) map[string]any {
	return map[string]any{"type": "integer", "minimum": minimum, "maximum": 9007199254740991}
}
func openAPIWorkbenchSuggestionSchemas() map[string]any {
	endpoint := closedChildObject([]string{}, map[string]any{"ref": schemaRef("WorkbenchNodeRef"), "evidence": schemaRef("SuggestionEvidence"), "creation": closedChildObject([]string{"sourceBindingId", "requestId"}, map[string]any{"sourceBindingId": sessionString(64), "requestId": sessionString(128)})})
	endpoint["oneOf"] = []map[string]any{metadataOperationVariant([]string{"ref", "evidence"}, "creation"), metadataOperationVariant([]string{"creation"}, "ref", "evidence")}
	decision := closedChildObject([]string{"selection", "key", "decision"}, map[string]any{"selection": schemaRef("SuggestionSelection"), "key": workbenchDigest(), "decision": workbenchEnum("accept", "reject"), "reason": sessionString(4096), "expectedOwner": schemaRef("MetadataRevision"), "expectedOperationDigest": workbenchDigest()})
	decision["oneOf"] = []map[string]any{
		{"properties": map[string]any{"decision": map[string]any{"const": "accept"}}, "required": []string{"expectedOwner", "expectedOperationDigest"}},
		{"properties": map[string]any{"decision": map[string]any{"const": "reject"}}, "not": map[string]any{"anyOf": []map[string]any{{"required": []string{"expectedOwner"}}, {"required": []string{"expectedOperationDigest"}}}}},
	}

	return map[string]any{
		"SuggestionSelection":       closedChildObject([]string{"runId", "sequence"}, map[string]any{"runId": map[string]any{"type": "string", "pattern": "^[0-9a-f]{32}$"}, "sequence": suggestionSequence(1)}),
		"SuggestionArtifact":        closedChildObject([]string{"sequence", "stageSequence", "stageId", "attempt", "branch", "name", "digest", "bytes"}, map[string]any{"sequence": suggestionSequence(1), "stageSequence": suggestionSequence(1), "stageId": sessionString(128), "attempt": suggestionSequence(1), "branch": suggestionSequence(0), "name": sessionString(256), "digest": workbenchDigest(), "bytes": map[string]any{"type": "integer", "minimum": 1, "maximum": 256 << 10}}),
		"SuggestionInventory":       closedChildObject([]string{"runId", "artifacts", "partial"}, map[string]any{"runId": sessionString(32), "artifacts": workbenchArray("SuggestionArtifact", 32), "nextSequence": suggestionSequence(1), "partial": map[string]any{"type": "boolean"}}),
		"SuggestionEvidence":        closedChildObject([]string{"sourceTargetDigest"}, map[string]any{"sourceTargetDigest": workbenchDigest(), "nativeRevision": sessionString(512), "nativeLocator": map[string]any{"type": "string", "pattern": "^[1-9][0-9]*$", "maxLength": 19}, "path": sessionString(1024), "repositoryRevision": schemaRef("MetadataRevision")}),
		"SuggestionEndpoint":        endpoint,
		"BoundSuggestion":           closedChildObject([]string{"key", "proposal", "origin"}, map[string]any{"key": workbenchDigest(), "proposal": closedChildObject([]string{"kind", "from", "to", "rationale"}, map[string]any{"kind": workbenchEnum("references", "contributes-to", "parent-of", "blocked-by", "milestone-member", "implemented-by"), "from": schemaRef("SuggestionEndpoint"), "to": schemaRef("SuggestionEndpoint"), "rationale": sessionString(4096)}), "origin": closedChildObject([]string{"runId", "stageId", "attempt", "artifactPath", "artifactDigest"}, map[string]any{"runId": sessionString(32), "stageId": sessionString(128), "attempt": suggestionSequence(1), "artifactPath": sessionString(1024), "artifactDigest": workbenchDigest()})}),
		"SuggestionCandidate":       closedChildObject([]string{"suggestion", "supported"}, map[string]any{"suggestion": schemaRef("BoundSuggestion"), "supported": map[string]any{"type": "boolean"}, "reason": sessionString(64), "reviewId": sessionString(42), "reviewState": workbenchEnum("accepting", "linked", "rejected")}),
		"SuggestionBatch":           closedChildObject([]string{"selection", "artifact", "candidates", "omitted"}, map[string]any{"selection": schemaRef("SuggestionSelection"), "artifact": schemaRef("SuggestionArtifact"), "candidates": workbenchArray("SuggestionCandidate", 100), "omitted": map[string]any{"type": "integer", "minimum": 0, "maximum": 100}}),
		"SuggestionPreviewRequest":  closedChildObject([]string{"selection", "key"}, map[string]any{"selection": schemaRef("SuggestionSelection"), "key": workbenchDigest()}),
		"SuggestionPreview":         closedChildObject([]string{"sourceBindingId", "preview"}, map[string]any{"sourceBindingId": sessionString(64), "preview": schemaRef("MetadataPreview")}),
		"SuggestionDecisionRequest": decision,
		"SuggestionReview":          closedChildObject([]string{"id", "suggestion", "state", "decision", "acceptedAt", "duplicate"}, map[string]any{"id": sessionString(42), "suggestion": schemaRef("BoundSuggestion"), "state": workbenchEnum("accepting", "linked", "rejected"), "decision": workbenchEnum("accept", "reject"), "reason": sessionString(4096), "acceptedAt": dateTimeSchema(), "duplicate": map[string]any{"type": "boolean"}, "proposal": schemaRef("MetadataProposalCommand")}),
	}
}
func workbenchSuggestionResponses(id RouteID) map[string]any {
	names := map[RouteID]string{RouteWorkbenchSuggestionArtifacts: "SuggestionInventory", RouteWorkbenchSuggestionLoad: "SuggestionBatch", RouteWorkbenchSuggestionPreview: "SuggestionPreview", RouteWorkbenchSuggestionDecide: "SuggestionReview", RouteWorkbenchSuggestionReview: "SuggestionReview"}
	return map[string]any{"200": jsonResponse("Current authorized review evidence; suggestions are not accepted source relationships", schemaRef(names[id])), "default": jsonResponse("Structured API error", schemaRef("ErrorEnvelope"))}
}
func workbenchSuggestionRequestBody(id RouteID) map[string]any {
	names := map[RouteID]string{RouteWorkbenchSuggestionPreview: "SuggestionPreviewRequest", RouteWorkbenchSuggestionDecide: "SuggestionDecisionRequest"}
	if names[id] == "" {
		return nil
	}
	return map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": schemaRef(names[id])}}}
}
