package apicontract

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

func TestWorkbenchSuggestionClosedSchemasAndBoundedRoutes(t *testing.T) {
	f := newWireFixtures()
	raw, _ := json.Marshal(map[string]any{"components": map[string]any{"schemas": openAPIInteractiveSchemas()}})
	for name, value := range map[string]any{"SuggestionSelection": f.SuggestionSelection, "SuggestionInventory": f.SuggestionInventory, "SuggestionBatch": f.SuggestionBatch, "SuggestionPreviewRequest": f.SuggestionPreviewRequest, "SuggestionPreview": f.SuggestionPreview, "SuggestionDecisionRequest": f.SuggestionDecision, "SuggestionReview": f.SuggestionReview} {
		c := jsonschema.NewCompiler()
		if err := c.AddResource("suggestions.json", bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		schema, err := c.Compile("suggestions.json#/components/schemas/" + name)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(value)
		var object map[string]any
		if err = json.Unmarshal(body, &object); err != nil {
			t.Fatal(err)
		}
		if err = schema.Validate(object); err != nil {
			t.Fatal(name, err)
		}
		object["credentialRef"] = "automation"
		if schema.Validate(object) == nil {
			t.Fatal("authority accepted", name)
		}
	}
	count := 0
	for _, route := range V1Routes() {
		if workbenchSuggestionRoute(route.ID) {
			count++
			if route.Budget != BoundedBudget {
				t.Fatal(route)
			}
			if routeRequiresIdempotency(route.ID) {
				t.Fatal("review uses normalized candidate custody, not fresh request keys")
			}
		}
	}
	if count != 5 {
		t.Fatal(count)
	}
}
