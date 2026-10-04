package apicontract

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

func TestChildWorkflowContractClosedAndGrantRequired(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		raw, err := OpenAPIDocument(authenticated)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Paths map[string]map[string]struct {
				Security   []any `json:"security"`
				Parameters []struct {
					Name     string         `json:"name"`
					Required bool           `json:"required"`
					Schema   map[string]any `json:"schema"`
				} `json:"parameters"`
				Responses map[string]any `json:"responses"`
			} `json:"paths"`
			Components struct {
				Schemas map[string]json.RawMessage `json:"schemas"`
			} `json:"components"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		for _, id := range []RouteID{RouteChildWorkflowValidate, RouteChildWorkflowStart, RouteChildWorkflowStatus, RouteChildWorkflowResolve} {
			route, ok := V1Route(id)
			if !ok || route.Method != http.MethodPost || route.Cost != CostMutation || route.ActionClass != ActionWorkflowExecution || route.Budget != MutationBudget || InitiallyRemoteInvocable(id) {
				t.Fatalf("route=%+v", route)
			}
			operation := doc.Paths[route.Path]["post"]
			if len(operation.Security) != 1 {
				t.Fatalf("%s lacks signed grant in mode %t", id, authenticated)
			}
			hasKey := false
			for _, p := range operation.Parameters {
				if p.Name == "Idempotency-Key" {
					hasKey = p.Required && p.Schema["maxLength"] == float64(256)
				}
			}
			if hasKey != (id == RouteChildWorkflowStart) {
				t.Fatalf("%s key=%t", id, hasKey)
			}
			code := "200"
			if id == RouteChildWorkflowStart || id == RouteChildWorkflowResolve {
				code = "202"
			}
			if operation.Responses[code] == nil || operation.Responses["204"] != nil {
				t.Fatalf("%s responses=%v", id, operation.Responses)
			}
		}
		for name, field := range map[string]string{"ChildWorkflowSourceRequest": "source", "ChildWorkflowStatusRequest": "invocationKey"} {
			schema, err := jsonschema.CompileString("request.json", string(doc.Components.Schemas[name]))
			if err != nil {
				t.Fatal(err)
			}
			value := map[string]any{field: "value"}
			if err := schema.Validate(value); err != nil {
				t.Fatal(err)
			}
			for _, authority := range []string{"actor", "origin", "policy", "runId", "grant"} {
				value[authority] = "forged"
				if err := schema.Validate(value); err == nil {
					t.Fatalf("%s accepted %s", name, authority)
				}
				delete(value, authority)
			}
		}
	}
}

func TestChildWorkflowWireMatchesClosedSchemas(t *testing.T) {
	schemas := openAPIChildWorkflowSchemas()
	document := map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "components": map[string]any{"schemas": schemas}}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	fixtures := newWireFixtures()
	for name, value := range map[string]any{"ChildWorkflowPage": fixtures.ChildWorkflowPage, "ChildPublicationCheckRequest": fixtures.ChildPublicationCheck, "ChildPublicationCheckResult": fixtures.ChildPublicationResult, "ChildWorkflowSourceRequest": fixtures.ChildWorkflowSource, "ChildWorkflowStatusRequest": fixtures.ChildWorkflowStatus, "ChildWorkflowValidationResponse": fixtures.ChildWorkflowValidation, "ChildWorkflowResponse": fixtures.ChildWorkflow, "ChildWorkflowResolveRequest": fixtures.ChildWorkflowResolve, "ChildWorkflowResolutionResponse": fixtures.ChildWorkflowResolution} {
		compiler := jsonschema.NewCompiler()
		if err := compiler.AddResource("fixture.json", bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		schema, err := compiler.Compile("fixture.json#/components/schemas/" + name)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var data any
		if err := json.Unmarshal(encoded, &data); err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(data); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
