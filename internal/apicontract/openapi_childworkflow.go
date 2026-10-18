package apicontract

func parentAccessRoute(id RouteID) bool {
	return id == RouteChildWorkflowAccessAcquire || id == RouteChildWorkflowAccessRevoke
}

func childWorkflowRoute(id RouteID) bool {
	return id == RouteChildWorkflowValidate || id == RouteChildWorkflowStart || id == RouteChildWorkflowStatus || id == RouteChildWorkflowResolve
}

func openAPIChildWorkflowSchemas() map[string]any {
	digest := map[string]any{"type": "string", "pattern": "^sha256:[0-9a-f]{64}$"}
	key := map[string]any{"type": "string", "minLength": 1, "maxLength": MaxChildWorkflowInvocationKeyBytes, "x-goobers-max-bytes": MaxChildWorkflowInvocationKeyBytes}
	action := map[string]any{"type": "string", "enum": []string{"merge", "replace", "discard"}}
	digests := map[string]any{"sourceDigest": digest, "canonicalDigest": digest, "configDigest": digest, "policyDigest": digest, "workflowDigest": digest}
	validation := mergeSchemaProperties(digests, map[string]any{
		"valid": map[string]any{"type": "boolean"}, "advisory": map[string]any{"type": "boolean", "const": true},
		"diagnostics": map[string]any{"type": "array", "items": schemaRef("ChildWorkflowDiagnostic")},
	})
	custody := mergeSchemaProperties(digests, map[string]any{
		"childId": stringSchema(), "acceptanceId": stringSchema(), "runId": stringSchema(), "invocationKey": key,
		"sequence":  map[string]any{"type": "integer", "minimum": 1},
		"state":     map[string]any{"type": "string", "enum": []string{"queued", "running", "awaiting_human", "completed", "failed", "cancelled"}},
		"duplicate": map[string]any{"type": "boolean"}, "cancellationRequested": map[string]any{"type": "boolean"},
		"acknowledged": map[string]any{"type": "boolean"}, "disposition": schemaRef("ChildWorkflowResolutionResponse"),
		"resultRef": stringSchema(), "workspaceRef": stringSchema(), "acceptedAt": dateTimeSchema(), "updatedAt": dateTimeSchema(),
	})
	return map[string]any{
		"ChildWorkflowAccessRequest":  closedChildObject([]string{"contractDigest"}, map[string]any{"contractDigest": digest}),
		"ChildWorkflowAccessResponse": closedChildObject([]string{"endpoint", "bearerToken"}, map[string]any{"endpoint": stringSchema(), "bearerToken": map[string]any{"type": "string", "description": "Short-lived secret delivered only to the authenticated parent attempt"}}),
		"ChildWorkflowSourceRequest": closedChildObject([]string{"source"}, map[string]any{
			"source": map[string]any{"type": "string", "minLength": 1, "maxLength": MaxChildWorkflowSourceBytes,
				"x-goobers-max-bytes": MaxChildWorkflowSourceBytes, "description": "UTF-8 Workflow DSL; at most 1 MiB after JSON decoding. No policy or origin is accepted in the request."},
		}),
		"ChildWorkflowStatusRequest":      closedChildObject([]string{"invocationKey"}, map[string]any{"invocationKey": key}),
		"ChildWorkflowResolveRequest":     closedChildObject([]string{"invocationKey", "action", "resultRef"}, map[string]any{"invocationKey": key, "action": action, "resultRef": digest, "expectedRequestDigest": digest}),
		"ChildWorkflowResolutionResponse": closedChildObject([]string{"invocationKey", "action", "resultRef", "requestedAt", "applied", "requestDigest", "planPublished"}, map[string]any{"invocationKey": key, "action": action, "resultRef": digest, "requestedAt": dateTimeSchema(), "applied": map[string]any{"type": "boolean"}, "appliedAt": dateTimeSchema(), "requestDigest": digest, "planPublished": map[string]any{"type": "boolean"}}),
		"ChildWorkflowDiagnostic": closedChildObject([]string{"code", "message"}, map[string]any{
			"code": stringSchema(), "stage": stringSchema(), "field": stringSchema(), "message": stringSchema(),
		}),
		"ChildWorkflowValidationResponse": closedChildObject([]string{"valid", "advisory", "diagnostics"}, validation),
		"ChildWorkflowResponse":           closedChildObject([]string{"childId", "acceptanceId", "runId", "invocationKey", "sequence", "state", "sourceDigest", "canonicalDigest", "configDigest", "policyDigest", "workflowDigest", "cancellationRequested", "acknowledged", "acceptedAt", "updatedAt"}, custody),
	}
}

func closedChildObject(required []string, properties map[string]any) map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": properties}
}
