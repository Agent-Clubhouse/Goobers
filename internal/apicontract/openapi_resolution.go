package apicontract

import "github.com/goobers/goobers/internal/sessioning"

func needsHumanEvidenceSchema() map[string]any {
	return closedChildObject([]string{"kind", "id", "digest"}, map[string]any{"kind": map[string]any{"type": "string", "enum": []string{"current-human-message", "learned-record", "session-message", "native-command", "source-comment"}}, "id": sessionString(128), "digest": workbenchDigest()})
}
func needsHumanAssessmentSchema() map[string]any {
	basis := needsHumanEvidenceSchema()
	basis["properties"].(map[string]any)["kind"] = map[string]any{"type": "string", "enum": []string{"current-human-message", "learned-record"}}
	return closedChildObject([]string{"id", "sourceId", "expectedRevision", "observationDigest", "basis", "rationale", "evidence"}, map[string]any{"id": sessionString(19), "sourceId": sessionString(19), "expectedRevision": sessionString(256), "observationDigest": workbenchDigest(), "basis": basis, "rationale": sessionString(8192), "evidence": map[string]any{"type": "array", "minItems": 1, "maxItems": 8, "items": schemaRef("NeedsHumanEvidenceRef")}})
}
func resolutionArray(name string, maximum int) map[string]any {
	return map[string]any{"type": "array", "maxItems": maximum, "items": schemaRef(name)}
}
func openAPIResolutionSchemas() map[string]any {
	schemas := map[string]any{
		"NeedsHumanEvidenceRef": needsHumanEvidenceSchema(), "NeedsHumanAssessment": needsHumanAssessmentSchema(),
		"NeedsHumanComment":           closedChildObject([]string{"id", "text", "digest"}, map[string]any{"id": sessionString(128), "author": sessionString(512), "text": sessionString(64 << 10), "digest": workbenchDigest(), "createdAt": dateTimeSchema()}),
		"NeedsHumanDependency":        closedChildObject([]string{"id", "open", "verified"}, map[string]any{"id": sessionString(128), "sourceId": sessionString(512), "revision": sessionString(256), "open": map[string]any{"type": "boolean"}, "verified": map[string]any{"type": "boolean"}}),
		"NeedsHumanObservation":       closedChildObject([]string{"digest", "item", "markerPresent", "comments", "dependencies", "commentsComplete", "dependenciesComplete", "learnedRecordDigest", "learnedDependencies", "learnedComplete", "waitReasons"}, map[string]any{"digest": workbenchDigest(), "humanInstruction": schemaRef("NeedsHumanEvidenceRef"), "item": schemaRef("BacklogItem"), "markerPresent": map[string]any{"type": "boolean"}, "comments": resolutionArray("NeedsHumanComment", 100), "dependencies": resolutionArray("NeedsHumanDependency", 64), "commentsComplete": map[string]any{"type": "boolean"}, "dependenciesComplete": map[string]any{"type": "boolean"}, "learnedRecordDigest": workbenchDigest(), "learnedReason": sessionString(8192), "learnedDependencies": resolutionArray("NeedsHumanDependency", 64), "learnedComplete": map[string]any{"type": "boolean"}, "waitReasons": workbenchStrings(16, 128)}),
		"NeedsHumanResolutionOrigin":  closedChildObject([]string{"runId", "sessionId", "turnId", "messageId", "messageDigest", "gooberDigest"}, map[string]any{"runId": map[string]any{"type": "string", "pattern": "^[0-9a-f]{32}$"}, "sessionId": sessionString(128), "turnId": sessionString(128), "messageId": sessionString(128), "messageDigest": workbenchDigest(), "gooberDigest": map[string]any{"type": "string", "pattern": "^sha256:[0-9a-f]{64}$"}}),
		"NeedsHumanResolutionReceipt": closedChildObject([]string{"operationDigest", "outcome", "revisionSemantics", "providerAcknowledged", "observedClear"}, map[string]any{"operationDigest": workbenchDigest(), "outcome": map[string]any{"type": "string", "enum": []string{"confirmed", "not-applied", "unknown"}}, "revisionSemantics": workbenchRevisionSemantics(), "providerAcknowledged": map[string]any{"type": "boolean"}, "observedClear": map[string]any{"type": "boolean"}, "observed": schemaRef("BacklogItem"), "waitReasons": workbenchStrings(16, 128)}),
	}
	schemas["NeedsHumanResolutionCommand"] = needsHumanCommandSchema()
	return schemas
}
func needsHumanCommandSchema() map[string]any {
	return closedChildObject([]string{"assessment", "id", "gaggle", "sourceBindingId", "actor", "itemId", "sourceId", "state", "duplicate", "requestDigest", "operationDigest", "origin", "acceptedAt", "nextAction"}, map[string]any{"assessment": schemaRef("NeedsHumanAssessment"), "id": map[string]any{"type": "string", "pattern": "^resolution-[0-9a-f]{32}$"}, "gaggle": sessionString(128), "sourceBindingId": sessionString(64), "actor": schemaRef("WorkbenchCommandActor"), "itemId": sessionString(19), "sourceId": sessionString(19), "state": map[string]any{"type": "string", "enum": []string{"accepted", "attempting", "confirmed", "not-applied", "unknown"}}, "duplicate": map[string]any{"type": "boolean"}, "requestDigest": workbenchDigest(), "operationDigest": workbenchDigest(), "origin": schemaRef("NeedsHumanResolutionOrigin"), "acceptedAt": dateTimeSchema(), "attemptedAt": dateTimeSchema(), "completedAt": dateTimeSchema(), "receipt": schemaRef("NeedsHumanResolutionReceipt"), "nextAction": sessionString(1024)})
}
func sessionResolutionBody(id RouteID) map[string]any {
	props := map[string]any{"sourceBindingId": sessionString(64)}
	required := []string{"sourceBindingId"}
	limit := sessioning.MaxOperationRequestBytes
	schema := closedChildObject(required, props)
	switch id {
	case RouteSessionNeedsHumanInspect:
		props["id"] = sessionString(19)
		props["expectedSourceId"] = sessionString(19)
		schema["required"] = []string{"sourceBindingId", "id", "expectedSourceId"}
	case RouteSessionNeedsHumanResolve:
		schema = needsHumanAssessmentSchema()
		props = schema["properties"].(map[string]any)
		props["sourceBindingId"] = sessionString(64)
		props["requestId"] = sessionString(128)
		schema["required"] = append(schema["required"].([]string), "sourceBindingId", "requestId")
		limit = sessioning.MaxOperationResolutionBytes
	case RouteSessionNeedsHumanReceipt:
		props["commandId"] = map[string]any{"type": "string", "pattern": "^resolution-[0-9a-f]{32}$"}
		schema["required"] = []string{"sourceBindingId", "commandId"}
	}
	return map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": schema}}, "description": "One dedicated, inspected marker operation under the actual current human session lease. Evidence references confer no authority. A current-human-message basis must refer to that human's explicit resolution instruction; the agent records the semantic assessment.", "x-goobers-max-bytes": limit}
}
func resolutionOperationRoute(id RouteID) bool {
	switch id {
	case RouteSessionNeedsHumanInspect, RouteSessionNeedsHumanResolve, RouteSessionNeedsHumanReceipt:
		return true
	}
	return false
}
