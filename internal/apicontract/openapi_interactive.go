package apicontract

func openAPIInteractiveSchemas() map[string]any {
	schemas := mergeSchemaProperties(mergeSchemaProperties(openAPIInteractiveRunSchemas(), openAPISessionSchemas()), openAPIWorkbenchSchemas())
	schemas = mergeSchemaProperties(schemas, openAPIEventIngressSchemas())
	schemas = mergeSchemaProperties(schemas, openAPIStartQueueSchemas())
	schemas = mergeSchemaProperties(schemas, openAPIWorkbenchGraphSchemas())
	schemas = mergeSchemaProperties(schemas, openAPIWorkbenchWriteSchemas())
	schemas = mergeSchemaProperties(schemas, openAPIResolutionSchemas())
	schemas = mergeSchemaProperties(schemas, openAPIWorkbenchProposalSchemas())
	schemas = mergeSchemaProperties(schemas, openAPIPRRepairSchemas())
	for name, schema := range map[string]any{
		"InteractiveCapabilities": closedChildObject([]string{"gaggle", "policyConfigured", "viewer", "operator", "sourceWriteMode", "actions"}, map[string]any{
			"gaggle": stringSchema(), "policyConfigured": map[string]any{"type": "boolean"}, "viewer": map[string]any{"type": "boolean"}, "operator": map[string]any{"type": "boolean"}, "sourceWriteMode": map[string]any{"type": "string", "const": "pull-request"},
			"actions": map[string]any{"type": "array", "maxItems": 10, "items": schemaRef("InteractiveActionPermission")},
		}),
		"InteractiveActionPermission": closedChildObject([]string{"action", "authorized", "credentialConfigured", "available", "reasonCode"}, map[string]any{
			"action":     map[string]any{"type": "string", "enum": []string{"session.create", "session.message", "backlog.read", "backlog.edit", "backlog.resolve", "repository.read", "run.intervene", "run.restartStage", "pr.repair", "source.proposeChange", "queue.cancel"}},
			"authorized": map[string]any{"type": "boolean"}, "credentialConfigured": map[string]any{"type": "boolean"}, "available": map[string]any{"type": "boolean"},
			"reasonCode": map[string]any{"type": "string", "enum": []string{"policy_missing", "action_not_authorized", "credential_not_configured", "operation_not_implemented", ""}},
		}),
	} {
		schemas[name] = schema
	}
	return schemas
}
