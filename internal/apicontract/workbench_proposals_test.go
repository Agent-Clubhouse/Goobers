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
