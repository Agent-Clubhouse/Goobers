package apicontract

func sessionRepairTargetSchema() map[string]any {
	return closedChildObject([]string{"sourceBindingId", "repository", "repositorySourceId", "id", "sourceId", "expectedHeadSha"}, map[string]any{
		"sourceBindingId":    map[string]any{"type": "string", "pattern": `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`},
		"repository":         schemaRef("SessionRepairRepository"),
		"repositorySourceId": map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
		"id":                 map[string]any{"type": "string", "pattern": `^[1-9][0-9]{0,18}$`},
		"sourceId":           map[string]any{"type": "string", "pattern": `^[1-9][0-9]{0,18}$`},
		"expectedHeadSha":    map[string]any{"type": "string", "pattern": `^([0-9a-f]{40}|[0-9a-f]{64})$`},
	})
}

func sessionRepairRepositorySchema() map[string]any {
	return closedChildObject([]string{"provider", "owner", "name"}, map[string]any{
		"provider": map[string]any{"type": "string", "enum": []string{"github", "ado"}},
		"owner":    map[string]any{"type": "string", "minLength": 1, "maxLength": 256},
		"project":  map[string]any{"type": "string", "minLength": 1, "maxLength": 256},
		"name":     map[string]any{"type": "string", "minLength": 1, "maxLength": 256},
	})
}
