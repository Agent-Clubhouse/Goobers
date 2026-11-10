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
	}
}
