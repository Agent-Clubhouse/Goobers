package apicontract

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

func TestWorkbenchWriteSchemasValidateClosedFixtures(t *testing.T) {
	schemas := mergeSchemaProperties(openAPIWorkbenchSchemas(), openAPIWorkbenchWriteSchemas())
	raw, err := json.Marshal(map[string]any{"components": map[string]any{"schemas": schemas}})
	if err != nil {
		t.Fatal(err)
	}
	f := newWireFixtures()
	for name, value := range map[string]any{"BacklogPatchInput": f.WorkbenchPatch, "BacklogWriteCapabilities": f.WorkbenchWriteCapabilities, "BacklogEditCommand": f.WorkbenchCommand} {
		c := jsonschema.NewCompiler()
		if err = c.AddResource("workbench-write.json", bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		schema, err := c.Compile("workbench-write.json#/components/schemas/" + name)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(value)
		var object map[string]any
		if err = json.Unmarshal(encoded, &object); err != nil {
			t.Fatal(err)
		}
		if err = schema.Validate(object); err != nil {
			t.Fatal(name, err)
		}
		object["credentialRef"] = "automation"
		if schema.Validate(object) == nil {
			t.Fatal(name, "permits authority injection")
		}
	}
}
func TestWorkbenchWriteRoutesRequireHumanAndExactCommandContract(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		raw, err := OpenAPIDocument(authenticated)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Paths map[string]map[string]struct {
				Security   []any
				Parameters []struct {
					Name     string
					Required bool
				}
				Responses map[string]any
			}
		}
		if err = json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		for _, route := range V1Routes() {
			if !workbenchWriteRoute(route.ID) {
				continue
			}
			op := doc.Paths[route.Path][strings.ToLower(route.Method)]
			if len(op.Security) != 1 || op.Responses["200"] == nil {
				t.Fatal("wrong command contract", route.ID)
			}
			key := false
			for _, p := range op.Parameters {
				if p.Name == "Idempotency-Key" {
					key = p.Required
				}
			}
			if key != (route.Method == http.MethodPatch) {
				t.Fatal("wrong idempotency contract", route.ID)
			}
		}
	}
}
