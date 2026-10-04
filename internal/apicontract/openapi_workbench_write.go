package apicontract

func workbenchWriteFields() map[string]any {
	return map[string]any{"type": "string", "enum": []string{"title", "description", "state", "labels", "assignees"}}
}
func workbenchRevisionSemantics() map[string]any {
	return map[string]any{"type": "string", "enum": []string{"timestamp-preflight", "atomic-revision-test"}}
}
func workbenchDigest() map[string]any {
	return map[string]any{"type": "string", "pattern": "^[0-9a-f]{64}$"}
}
func openAPIWorkbenchWriteSchemas() map[string]any {
	input := closedChildObject([]string{"sourceId", "expectedRevision", "field"}, map[string]any{
		"sourceId": map[string]any{"type": "string", "pattern": "^[1-9][0-9]*$", "maxLength": 19}, "expectedRevision": map[string]any{"type": "string", "minLength": 1, "maxLength": 256}, "field": workbenchWriteFields(), "value": sessionString(192 << 10), "values": workbenchStrings(128, 400),
	})
	input["oneOf"] = []map[string]any{
		{"required": []string{"value"}, "not": map[string]any{"required": []string{"values"}}, "properties": map[string]any{"field": map[string]any{"enum": []string{"title", "description", "state"}}}},
		{"required": []string{"values"}, "not": map[string]any{"required": []string{"value"}}, "properties": map[string]any{"field": map[string]any{"enum": []string{"labels", "assignees"}}}},
	}
	return map[string]any{
		"BacklogPatchInput": input,
		"BacklogWriteCapabilities": closedChildObject([]string{"fields", "relationships", "revisionSemantics", "maxAssignees", "controlLabelChanges"}, map[string]any{
			"fields": map[string]any{"type": "array", "maxItems": 5, "uniqueItems": true, "items": workbenchWriteFields()}, "relationships": map[string]any{"type": "array", "maxItems": 0, "items": stringSchema()}, "revisionSemantics": workbenchRevisionSemantics(), "maxAssignees": map[string]any{"type": "integer", "enum": []int{1, 10}}, "controlLabelChanges": map[string]any{"type": "boolean", "const": false},
		}),
		"BacklogPatchReceipt": closedChildObject([]string{"operationDigest", "outcome", "revisionSemantics", "providerAcknowledged", "observedMatches"}, map[string]any{
			"operationDigest": workbenchDigest(), "outcome": map[string]any{"type": "string", "enum": []string{"confirmed", "not-applied", "unknown"}}, "revisionSemantics": workbenchRevisionSemantics(), "providerAcknowledged": map[string]any{"type": "boolean"}, "observedMatches": map[string]any{"type": "boolean"}, "observed": schemaRef("BacklogItem"),
		}),
		"WorkbenchCommandActor": closedChildObject([]string{"issuer", "subject"}, map[string]any{"issuer": sessionString(2048), "subject": sessionString(512)}),
		"BacklogEditCommand": closedChildObject([]string{"id", "gaggle", "sourceBindingId", "actor", "itemId", "sourceId", "field", "state", "duplicate", "requestDigest", "operationDigest", "acceptedAt", "nextAction"}, map[string]any{
			"id": map[string]any{"type": "string", "pattern": "^workbench-[0-9a-f]{32}$"}, "gaggle": sessionString(128), "sourceBindingId": sessionString(64), "actor": schemaRef("WorkbenchCommandActor"), "itemId": sessionString(19), "sourceId": sessionString(19), "field": workbenchWriteFields(), "state": map[string]any{"type": "string", "enum": []string{"accepted", "attempting", "confirmed", "not-applied", "unknown"}}, "duplicate": map[string]any{"type": "boolean"}, "requestDigest": workbenchDigest(), "operationDigest": workbenchDigest(), "acceptedAt": dateTimeSchema(), "attemptedAt": dateTimeSchema(), "completedAt": dateTimeSchema(), "receipt": schemaRef("BacklogPatchReceipt"), "nextAction": sessionString(1024),
		}),
	}
}
func workbenchWriteResponses(id RouteID) map[string]any {
	name := "BacklogEditCommand"
	if id == RouteWorkbenchWriteCapabilities {
		name = "BacklogWriteCapabilities"
	}
	return map[string]any{"200": jsonResponse("Current authorized command evidence or native capabilities; uncertainty is not success", schemaRef(name)), "default": jsonResponse("Structured API error", schemaRef("ErrorEnvelope"))}
}

// workbenchResponses keeps the source read/write family separate from generic
// run and daemon responses without changing any route's status or schema.
func workbenchResponses(id RouteID) map[string]any {
	if workbenchProposalRoute(id) {
		return workbenchProposalResponses(id)
	}
	if id == RouteWorkbenchGraph {
		return map[string]any{"200": jsonResponse("Current server-authorized graph; coverage and conflicts remain explicit", schemaRef("WorkbenchGraph")), "default": jsonResponse("Structured API error", schemaRef("ErrorEnvelope"))}
	}
	if workbenchWriteRoute(id) {
		return workbenchWriteResponses(id)
	}
	if workbenchReadRoute(id) {
		return workbenchReadResponses(id)
	}
	return nil
}
