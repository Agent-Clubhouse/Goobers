package apicontract

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

func TestWorkbenchProposalClosedSchemasAndRoutes(t *testing.T) {
	schemas := openAPIInteractiveSchemas()
	raw, err := json.Marshal(map[string]any{"components": map[string]any{"schemas": schemas}})
	if err != nil {
		t.Fatal(err)
	}
	f := newWireFixtures()
	for name, value := range map[string]any{"MetadataChangeRequest": f.MetadataChange, "MetadataPreview": f.MetadataPreview, "MetadataProposalCommand": f.MetadataProposal} {
		compiler := jsonschema.NewCompiler()
		if err = compiler.AddResource("proposal.json", bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		schema, err := compiler.Compile("proposal.json#/components/schemas/" + name)
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
			t.Fatal("authority injection", name)
		}
	}
	count := 0
	for _, route := range V1Routes() {
		if !workbenchProposalRoute(route.ID) {
			continue
		}
		count++
		if route.Budget != BoundedBudget {
			t.Fatal("route budget", route)
		}
		if routeRequiresIdempotency(route.ID) != (route.ID == RouteWorkbenchProposalSubmit) {
			t.Fatal("wrong command key", route.ID)
		}
	}
	if count != 5 {
		t.Fatal("missing proposal routes", count)
	}
}

func TestWorkbenchMetadataIdentitySchemasMatchWireAndRejectCombinedOperations(t *testing.T) {
	schemas := openAPIInteractiveSchemas()
	encoded, _ := json.Marshal(map[string]any{"components": map[string]any{"schemas": schemas}})
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("identity.json", bytes.NewReader(encoded)); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("identity.json#/components/schemas/MetadataChangeRequest")
	if err != nil {
		t.Fatal(err)
	}
	fixtures := newWireFixtures()
	for _, fixture := range []MetadataChangeRequest{fixtures.MetadataObjective, fixtures.MetadataAlias} {
		raw, _ := json.Marshal(fixture)
		var value map[string]any
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(value); err != nil {
			t.Fatal(err)
		}
		value["field"], value["value"] = "description", "body"
		if schema.Validate(value) == nil {
			t.Fatal("combined metadata operations accepted")
		}
		delete(value, "field")
		delete(value, "value")
		key := "objective"
		if fixture.Alias != nil {
			key = "alias"
		}
		value[key].(map[string]any)["credentialRef"] = "automation"
		if schema.Validate(value) == nil {
			t.Fatal("nested authority injection accepted")
		}
	}
}
