package apicontract

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
		"ChildWorkflowPage": closedChildObject([]string{"runId", "gaggle", "children"}, map[string]any{
			"publications": map[string]any{"type": "array", "maxItems": 2, "items": schemaRef("ChildPublicationSummary")}, "publicationCheckAvailable": map[string]any{"type": "boolean"}, "publicationCheckReason": stringSchema(),
			"runId": stringSchema(), "gaggle": stringSchema(), "parent": schemaRef("ChildWorkflowParent"), "nextCursor": map[string]any{"type": "string", "maxLength": 128},
			"children": map[string]any{"type": "array", "maxItems": 50, "items": schemaRef("ChildWorkflowSummary")},
		}),
		"ChildWorkflowParent": closedChildObject([]string{"runId", "workflow", "invocationKey"}, map[string]any{"runId": stringSchema(), "workflow": stringSchema(), "invocationKey": stringSchema()}),
		"ChildPublicationSummary": closedChildObject([]string{"action", "intentDigest", "state", "head", "base", "commit", "needsHuman", "createdAt", "updatedAt", "observation"}, map[string]any{
			"action": map[string]any{"type": "string", "enum": []string{"branch", "pr"}}, "intentDigest": digest,
			"state": map[string]any{"type": "string", "enum": []string{"prepared", "effect_pending", "confirmed"}}, "head": stringSchema(), "base": stringSchema(), "commit": stringSchema(),
			"checkedAt": dateTimeSchema(), "observation": stringSchema(), "pullRequestUrl": stringSchema(), "pullRequestNumber": map[string]any{"type": "integer", "minimum": 0}, "needsHuman": map[string]any{"type": "boolean"}, "createdAt": dateTimeSchema(), "updatedAt": dateTimeSchema(),
		}),
		"ChildPublicationCheckRequest": closedChildObject([]string{"action", "expectedIntentDigest"}, map[string]any{"action": map[string]any{"type": "string", "enum": []string{"branch", "pr"}}, "expectedIntentDigest": digest}),
		"ChildPublicationCheckResult":  closedChildObject([]string{"runId", "requestId", "publication"}, map[string]any{"runId": stringSchema(), "requestId": stringSchema(), "publication": schemaRef("ChildPublicationSummary")}),
		"ChildWorkflowSummary": closedChildObject([]string{"childId", "runAvailable", "invocationKey", "sequence", "state", "cancellationRequested", "acknowledged", "expired", "acceptedAt", "updatedAt"}, map[string]any{
			"publicationNeedsHuman": map[string]any{"type": "boolean"}, "childId": stringSchema(), "runId": stringSchema(), "runAvailable": map[string]any{"type": "boolean"}, "stage": stringSchema(), "workflow": stringSchema(), "invocationKey": stringSchema(),
			"sequence": map[string]any{"type": "integer", "minimum": 1}, "state": custody["state"], "cancellationRequested": map[string]any{"type": "boolean"}, "acknowledged": map[string]any{"type": "boolean"}, "expired": map[string]any{"type": "boolean"}, "acceptedAt": dateTimeSchema(), "updatedAt": dateTimeSchema(),
		}),
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
