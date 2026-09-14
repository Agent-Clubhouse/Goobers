package apicontract

func openAPIRemoteReadSchemas() map[string]any {
	return map[string]any{
		"BuildMetadata": objectSchema(
			[]string{"version", "commit", "date"},
			map[string]any{
				"version": stringSchema(),
				"commit":  stringSchema(),
				"date":    stringSchema(),
			},
		),
		"InstanceIdentity": objectSchema(
			[]string{"name", "environment"},
			map[string]any{
				"name":        stringSchema(),
				"environment": map[string]any{"type": "string", "enum": []string{"dev", "staging", "prod"}},
			},
		),
		"Freshness": objectSchema(
			[]string{"observedAt", "definitionsLoadedAt", "journalUpdatedAt", "lastSchedulerTickAt", "lastTickAgeMillis"},
			map[string]any{
				"observedAt":          dateTimeSchema(),
				"definitionsLoadedAt": dateTimeSchema(),
				"journalUpdatedAt":    nullableSchema(dateTimeSchema()),
				"lastSchedulerTickAt": nullableSchema(dateTimeSchema()),
				"lastTickAgeMillis":   nullableSchema(map[string]any{"type": "integer", "minimum": 0}),
			},
		),
		"InstanceDocument": objectSchema(
			[]string{"apiVersion", "schemaVersion", "name", "environment", "instanceRoot", "ready", "status", "concurrency", "counts", "warnings", "memoryGateEnabled", "fsyncDisabled", "fleetEnrolled"},
			map[string]any{
				"apiVersion":    map[string]any{"type": "string", "const": PreferredAPIVersion},
				"schemaVersion": stringSchema(),
				"name":          stringSchema(),
				"environment":   map[string]any{"type": "string", "enum": []string{"dev", "staging", "prod"}},
				"computerName":  stringSchema(),
				"instanceRoot":  stringSchema(),
				"rootIdentity":  map[string]any{"type": "object", "additionalProperties": true},
				"ready":         map[string]any{"type": "boolean"},
				"status":        map[string]any{"type": "string", "enum": []string{"starting", "ready", "degraded"}},
				"concurrency": objectSchema(
					[]string{"activeRuns", "maxConcurrentRuns"},
					map[string]any{
						"activeRuns":        map[string]any{"type": "integer", "minimum": 0},
						"maxConcurrentRuns": map[string]any{"type": "integer", "minimum": 0},
					},
				),
				"counts": objectSchema(
					[]string{"gaggles", "goobers", "workflows", "activeRuns"},
					map[string]any{
						"gaggles":    map[string]any{"type": "integer", "minimum": 0},
						"goobers":    map[string]any{"type": "integer", "minimum": 0},
						"workflows":  map[string]any{"type": "integer", "minimum": 0},
						"activeRuns": map[string]any{"type": "integer", "minimum": 0},
					},
				),
				"warnings":          map[string]any{"type": "array", "items": map[string]any{"type": "object", "additionalProperties": true}},
				"memoryHighWater":   map[string]any{"type": "number", "minimum": 0, "maximum": 1},
				"memoryGateEnabled": map[string]any{"type": "boolean"},
				"fsyncDisabled":     map[string]any{"type": "boolean"},
				"fleetEnrolled":     map[string]any{"type": "boolean"},
				"readState":         map[string]any{"type": "object", "additionalProperties": true},
			},
		),
		"RunListDocument": objectSchema(
			[]string{"runs"},
			map[string]any{
				"runs":             map[string]any{"type": "array", "items": schemaRef("RunSummary")},
				"workflowActivity": map[string]any{"type": "array", "items": schemaRef("WorkflowRunActivity")},
				"nextCursor":       stringSchema(),
				"readState":        map[string]any{"type": "object", "additionalProperties": true},
			},
		),
		"WorkflowRunActivity": objectSchema(
			[]string{"gaggle", "workflow", "activeRuns"},
			map[string]any{
				"gaggle":     stringSchema(),
				"workflow":   stringSchema(),
				"activeRuns": map[string]any{"type": "integer", "minimum": 0},
			},
		),
		"RunSummary": objectSchema(
			[]string{"id", "workflow", "workflowVersion", "gaggle", "trigger", "phase", "terminal", "startedAt", "durationMillis", "lastActivityAt", "stale", "lastSeq", "repassCount", "retryCount", "policyRetryCount", "infraRetryCount", "noWork", "operator"},
			map[string]any{
				"id":               stringSchema(),
				"workflow":         stringSchema(),
				"workflowVersion":  map[string]any{"type": "integer", "minimum": 1},
				"workflowDigest":   stringSchema(),
				"gaggle":           stringSchema(),
				"trigger":          map[string]any{"type": "object", "additionalProperties": true},
				"phase":            stringSchema(),
				"terminal":         map[string]any{"type": "boolean"},
				"currentStage":     stringSchema(),
				"startedAt":        dateTimeSchema(),
				"finishedAt":       dateTimeSchema(),
				"durationMillis":   map[string]any{"type": "integer", "minimum": 0},
				"lastActivityAt":   dateTimeSchema(),
				"stale":            map[string]any{"type": "boolean"},
				"lastSeq":          map[string]any{"type": "integer", "minimum": 0},
				"repassCount":      map[string]any{"type": "integer", "minimum": 0},
				"retryCount":       map[string]any{"type": "integer", "minimum": 0},
				"policyRetryCount": map[string]any{"type": "integer", "minimum": 0},
				"infraRetryCount":  map[string]any{"type": "integer", "minimum": 0},
				"noWork":           map[string]any{"type": "boolean"},
				"terminalReason":   stringSchema(),
				"operator":         map[string]any{"type": "object", "additionalProperties": true},
			},
		),
	}
}

func objectSchema(required []string, properties map[string]any) map[string]any {
	return map[string]any{
		"type":                 "object",
		"required":             required,
		"properties":           properties,
		"additionalProperties": true,
	}
}

func stringSchema() map[string]any {
	return map[string]any{"type": "string"}
}

func dateTimeSchema() map[string]any {
	return map[string]any{"type": "string", "format": "date-time"}
}

func nullableSchema(schema map[string]any) map[string]any {
	return map[string]any{"anyOf": []any{schema, map[string]any{"type": "null"}}}
}
