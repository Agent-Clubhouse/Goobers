package apicontract

func childWorkflowRoute(id RouteID) bool {
	return id == RouteChildWorkflowValidate || id == RouteChildWorkflowStart || id == RouteChildWorkflowStatus
}

func openAPIChildWorkflowSchemas() map[string]any {
	digest := map[string]any{"type": "string", "pattern": "^sha256:[0-9a-f]{64}$"}
	key := map[string]any{"type": "string", "minLength": 1, "maxLength": MaxChildWorkflowInvocationKeyBytes, "x-goobers-max-bytes": MaxChildWorkflowInvocationKeyBytes}
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
		"resultRef": stringSchema(), "workspaceRef": stringSchema(), "acceptedAt": dateTimeSchema(), "updatedAt": dateTimeSchema(),
	})
	return map[string]any{
		"ChildWorkflowSourceRequest": closedChildObject([]string{"source"}, map[string]any{
			"source": map[string]any{"type": "string", "minLength": 1, "maxLength": MaxChildWorkflowSourceBytes,
				"x-goobers-max-bytes": MaxChildWorkflowSourceBytes, "description": "UTF-8 Workflow DSL; at most 1 MiB after JSON decoding. No policy or origin is accepted in the request."},
		}),
		"ChildWorkflowStatusRequest": closedChildObject([]string{"invocationKey"}, map[string]any{"invocationKey": key}),
		"ChildWorkflowDiagnostic": closedChildObject([]string{"code", "message"}, map[string]any{
			"code": stringSchema(), "stage": stringSchema(), "field": stringSchema(), "message": stringSchema(),
		}),
		"ChildWorkflowValidationResponse": closedChildObject([]string{"valid", "advisory", "diagnostics"}, validation),
		"ChildWorkflowResponse":           closedChildObject([]string{"childId", "acceptanceId", "runId", "invocationKey", "sequence", "state", "sourceDigest", "canonicalDigest", "configDigest", "policyDigest", "workflowDigest", "cancellationRequested", "acceptedAt", "updatedAt"}, custody),
	}
}

func closedChildObject(required []string, properties map[string]any) map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": properties}
}
