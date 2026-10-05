package apicontract

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

func TestWorkbenchGraphWireSchemaIsClosedAndPreservesOwnership(t *testing.T) {
	schemas := openAPISchemas(true)
	raw, err := json.Marshal(map[string]any{"components": map[string]any{"schemas": schemas}})
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err = compiler.AddResource("graph.json", bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("graph.json#/components/schemas/WorkbenchGraph")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(workbenchGraphFixture())
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err = json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	if err = schema.Validate(value); err != nil {
		t.Fatal(err)
	}
	owner := value["edges"].([]any)[0].(map[string]any)["owner"].(map[string]any)
	if owner["kind"] != "manifest" || owner["sourceBindingId"] != "links" || owner["path"] != "links.yaml" {
		t.Fatal("source provenance missing", owner)
	}
	for _, object := range []map[string]any{value, owner, value["nodes"].([]any)[0].(map[string]any), value["edges"].([]any)[0].(map[string]any)["to"].(map[string]any)} {
		object["credentialOverride"] = "automation"
		if err = schema.Validate(value); err == nil {
			t.Fatal("graph accepted unknown authority field")
		}
		delete(object, "credentialOverride")
	}
	route, ok := V1Route(RouteWorkbenchGraph)
	if !ok || route.Method != http.MethodGet || route.Path != WorkbenchGraphPath || route.Budget != BoundedBudget {
		t.Fatal("graph route differs", route)
	}
	parameters := openAPIParameters(route)
	for _, p := range parameters {
		if p["in"] == "query" {
			t.Fatal("client selected graph sources", p)
		}
	}
	responses := openAPIResponses(route)
	if responses["200"] == nil {
		t.Fatal("graph success schema absent")
	}
}
