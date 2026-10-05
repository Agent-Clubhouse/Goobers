package apicontract

import "github.com/goobers/goobers/internal/sessioning"

func repairCommitSchema() map[string]any {
	return map[string]any{"type": "string", "pattern": "^([0-9a-f]{40}|[0-9a-f]{64})$"}
}
func repairCommandIDSchema() map[string]any {
	return map[string]any{"type": "string", "pattern": "^repair-[0-9a-f]{32}$"}
}
func prRepairRequestSchema() map[string]any {
	change := closedChildObject([]string{"path"}, map[string]any{"path": sessionString(1024), "previousBlob": repairCommitSchema(), "content": map[string]any{"type": "string", "maxLength": 1 << 20, "description": "New UTF-8 text; omit to delete. Empty string writes an empty file. Absence of previousBlob means add, subject to verified tree absence."}})
	return closedChildObject([]string{"requestId", "expectedHeadSha", "rationale", "changes"}, map[string]any{"requestId": sessionString(128), "expectedHeadSha": repairCommitSchema(), "parentCommandId": repairCommandIDSchema(), "rationale": sessionString(4096), "changes": map[string]any{"type": "array", "minItems": 1, "maxItems": 32, "items": change}})
}
func prRepairOperationRoute(id RouteID) bool {
	switch id {
	case RouteSessionPRRepairInspect, RouteSessionPRRepairRead, RouteSessionPRRepair, RouteSessionPRRepairReceipt:
		return true
	}
	return false
}
func prRepairOperationBody(id RouteID) map[string]any {
	schema := closedChildObject([]string{}, map[string]any{"parentCommandId": repairCommandIDSchema()})
	limit := sessioning.MaxOperationRequestBytes
	switch id {
	case RouteSessionPRRepairRead:
		schema["required"] = []string{"path"}
		schema["properties"].(map[string]any)["path"] = sessionString(1024)
	case RouteSessionPRRepair:
		schema = prRepairRequestSchema()
		limit = sessioning.MaxOperationPRRepairBytes
	case RouteSessionPRRepairReceipt:
		schema = closedChildObject([]string{"commandId"}, map[string]any{"commandId": repairCommandIDSchema()})
	}
	return map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": schema}}, "description": "Operation confined to the actual current human-selected PR. No caller repository, actor, credential or ownership claims are accepted. Repair is one native expected-head commit; PR-open state is checked before publication but is not atomic with the head CAS.", "x-goobers-max-bytes": limit}
}
func prRepairResponseName(id RouteID) string {
	switch id {
	case RouteSessionPRRepairInspect:
		return "SessionPRRepairInspection"
	case RouteSessionPRRepairRead:
		return "SessionPRRepairFile"
	}
	return "SessionPRRepairCommand"
}
func openAPIPRRepairSchemas() map[string]any {
	inspection := closedChildObject([]string{"target", "headSha", "baseSha", "head", "base", "title", "description", "url", "open", "draft"}, map[string]any{"target": schemaRef("SessionPRRepairTarget"), "parentCommandId": repairCommandIDSchema(), "headSha": repairCommitSchema(), "baseSha": repairCommitSchema(), "head": sessionString(1024), "base": sessionString(1024), "title": map[string]any{"type": "string", "maxLength": 8192}, "description": map[string]any{"type": "string", "maxLength": 64 << 10}, "url": map[string]any{"type": "string", "maxLength": 2048}, "open": map[string]any{"type": "boolean"}, "draft": map[string]any{"type": "boolean"}})
	file := closedChildObject([]string{"target", "headSha", "path", "present"}, map[string]any{"target": schemaRef("SessionPRRepairTarget"), "parentCommandId": repairCommandIDSchema(), "headSha": repairCommitSchema(), "path": sessionString(1024), "present": map[string]any{"type": "boolean"}, "blobId": repairCommitSchema(), "mode": map[string]any{"type": "string", "enum": []string{"100644", "100755"}}, "content": map[string]any{"type": "string", "maxLength": 1 << 20}})
	receipt := closedChildObject([]string{"operationDigest", "outcome", "mutationAttempted", "providerAcknowledged", "observedMatches"}, map[string]any{"operationDigest": workbenchDigest(), "outcome": map[string]any{"type": "string", "enum": []string{"confirmed", "not-applied", "unknown"}}, "mutationAttempted": map[string]any{"type": "boolean"}, "providerAcknowledged": map[string]any{"type": "boolean"}, "observedMatches": map[string]any{"type": "boolean"}, "commitId": repairCommitSchema()})
	command := closedChildObject([]string{"id", "sourceBindingId", "state", "requestDigest", "operationDigest", "selectedHeadSha", "expectedHeadSha", "runId", "actor", "acceptedAt"}, map[string]any{"id": repairCommandIDSchema(), "sourceBindingId": sessionString(128), "state": map[string]any{"type": "string", "enum": []string{"accepted", "attempting", "confirmed", "not-applied", "unknown", "observed-applied"}}, "requestDigest": workbenchDigest(), "operationDigest": workbenchDigest(), "selectedHeadSha": repairCommitSchema(), "expectedHeadSha": repairCommitSchema(), "parentCommandId": repairCommandIDSchema(), "runId": map[string]any{"type": "string", "pattern": "^[0-9a-f]{32}$"}, "actor": schemaRef("SessionActor"), "acceptedAt": dateTimeSchema(), "attemptedAt": dateTimeSchema(), "completedAt": dateTimeSchema(), "receipt": schemaRef("SessionPRRepairReceipt"), "observations": workbenchArray("PRRepairObservation", 16), "omittedObservations": map[string]any{"type": "integer", "minimum": 0}})
	return map[string]any{"PRRepairObservation": closedChildObject([]string{"checker", "at", "matches"}, map[string]any{"checker": closedChildObject([]string{"issuer", "subject"}, map[string]any{"issuer": sessionString(2048), "subject": sessionString(512)}), "at": dateTimeSchema(), "matches": map[string]any{"type": "boolean"}, "commitId": repairCommitSchema()}), "SessionPRRepairInspection": inspection, "SessionPRRepairFile": file, "SessionPRRepairReceipt": receipt, "SessionPRRepairCommand": command}
}
