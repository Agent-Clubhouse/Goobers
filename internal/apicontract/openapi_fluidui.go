package apicontract

// openAPILabsFluidUISchemas describes the Labs Fluid UI customization
// document (#8415). It mirrors internal/fluidui; the server remains the
// authority on bounds and rejects anything this schema would.
func openAPILabsFluidUISchemas() map[string]any {
	slug := map[string]any{"type": "string", "pattern": "^[a-z][a-z0-9-]{0,63}$"}
	slugs := func(max int) map[string]any {
		return map[string]any{"type": "array", "items": slug, "maxItems": max, "uniqueItems": true}
	}
	optionalObject := func(properties map[string]any) map[string]any {
		return map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	}
	return map[string]any{
		"PortalCustomization": strictObjectSchema(
			[]string{"version"},
			map[string]any{
				"version": map[string]any{"type": "string", "const": "portal.customization/v1"},
				"density": map[string]any{"type": "string", "enum": []string{"comfortable", "compact"}},
				"pages": map[string]any{
					"type": "object", "maxProperties": 32, "propertyNames": slug,
					"additionalProperties": optionalObject(map[string]any{"hidden": slugs(64), "order": slugs(64)}),
				},
				"runList": optionalObject(map[string]any{
					"sort":   map[string]any{"type": "string", "enum": []string{"newest", "oldest", "failing-first"}},
					"phases": slugs(16),
				}),
				"savedViews": map[string]any{
					"type": "array", "maxItems": 20,
					"items": strictObjectSchema([]string{"name", "route"}, map[string]any{
						"name":  map[string]any{"type": "string", "minLength": 1, "maxLength": 64},
						"route": map[string]any{"type": "string", "pattern": `^#/[^\s<>"\\]*$`, "maxLength": 512},
					}),
				},
			},
		),
		"PortalCustomizationState": objectSchema(
			[]string{"customization", "revision", "undoDepth"},
			map[string]any{
				"customization": nullableSchema(schemaRef("PortalCustomization")),
				"revision":      map[string]any{"type": "integer", "minimum": 0},
				"updatedAt":     dateTimeSchema(),
				"incompatible":  map[string]any{"type": "boolean"},
				"undoDepth":     map[string]any{"type": "integer", "minimum": 0},
			},
		),
		"FluidUIRequestSubmit": strictObjectSchema(
			[]string{"prompt"},
			map[string]any{"prompt": map[string]any{"type": "string", "minLength": 1, "maxLength": 2000}},
		),
		"FluidUIRequest": objectSchema(
			[]string{"id", "prompt", "status", "attempt", "createdAt", "updatedAt"},
			map[string]any{
				"id":     map[string]any{"type": "string", "pattern": "^fr-[0-9a-f]{16}$"},
				"prompt": map[string]any{"type": "string"},
				"status": map[string]any{"type": "string", "enum": []string{
					"queued", "implementing", "ready_to_preview", "applied", "failed", "unsupported", "reverted",
				}},
				"attempt":         map[string]any{"type": "integer", "minimum": 1},
				"runId":           map[string]any{"type": "string"},
				"candidate":       schemaRef("PortalCustomization"),
				"unmetNeed":       map[string]any{"type": "string", "pattern": "^[a-z0-9-]{1,48}$"},
				"errors":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"detail":          map[string]any{"type": "string"},
				"appliedRevision": map[string]any{"type": "integer", "minimum": 0},
				"createdAt":       dateTimeSchema(),
				"updatedAt":       dateTimeSchema(),
			},
		),
		"FluidUIRequestList": objectSchema(
			[]string{"requests"},
			map[string]any{"requests": map[string]any{"type": "array", "items": schemaRef("FluidUIRequest")}},
		),
		"FluidUIRequestApplication": objectSchema(
			[]string{"request", "customization"},
			map[string]any{
				"request":       schemaRef("FluidUIRequest"),
				"customization": schemaRef("PortalCustomizationState"),
			},
		),
		"PortalCustomizationReport": openAPILabsFluidUIReportSchema(),
	}
}

// openAPILabsFluidUIReportSchema describes the aggregate promotion-candidate
// report (#8419): counts, shapes, and need labels only.
func openAPILabsFluidUIReportSchema() map[string]any {
	count := map[string]any{"type": "integer", "minimum": 0}
	return objectSchema(
		[]string{"advisory", "generatedAt", "sustainedAfterDays", "users", "candidates", "unmetNeeds"},
		map[string]any{
			"advisory":           map[string]any{"type": "string"},
			"generatedAt":        dateTimeSchema(),
			"sustainedAfterDays": count,
			"users":              count,
			"candidates": map[string]any{"type": "array", "items": objectSchema(
				[]string{"rank", "shape", "users", "activeUsers", "sustainedUsers", "revertedUsers", "reversionRate"},
				map[string]any{
					"rank":           map[string]any{"type": "integer", "minimum": 1},
					"shape":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"users":          count,
					"activeUsers":    count,
					"sustainedUsers": count,
					"revertedUsers":  count,
					"reversionRate":  map[string]any{"type": "number", "minimum": 0, "maximum": 1},
				},
			)},
			"unmetNeeds": map[string]any{"type": "array", "items": objectSchema(
				[]string{"label", "users"},
				map[string]any{"label": map[string]any{"type": "string"}, "users": count},
			)},
		},
	)
}
