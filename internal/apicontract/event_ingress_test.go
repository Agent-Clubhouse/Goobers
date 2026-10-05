package apicontract

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

func TestGaggleEventWireSchemasAndAuthentication(t *testing.T) {
	raw, err := OpenAPIDocument(false)
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err = c.AddResource("ingress.json", bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	f := newWireFixtures()
	for name, value := range map[string]any{"GaggleEventEnvelope": f.GaggleEventEnvelope, "GaggleEventReceipt": f.GaggleEventReceipt} {
		schema, err := c.Compile("ingress.json#/components/schemas/" + name)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(value)
		var decoded map[string]any
		if err = json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		if err = schema.Validate(decoded); err != nil {
			t.Fatal(name, err)
		}
		decoded["actorOverride"] = "another"
		if err = schema.Validate(decoded); err == nil {
			t.Fatal("open authority schema", name)
		}
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
	for _, id := range []RouteID{RouteGaggleEventPublish, RouteGaggleEventReceipt} {
		route, ok := V1Route(id)
		if !ok {
			t.Fatal(id)
		}
		op := doc.Paths[route.Path][strings.ToLower(route.Method)]
		if len(op.Security) != 1 {
			t.Fatal("anonymous ingress contract", id)
		}
		if id == RouteGaggleEventPublish {
			found := false
			for _, p := range op.Parameters {
				if p.Name == EventBindingHeader && p.Required {
					found = true
				}
			}
			if !found || op.Responses["202"] == nil || route.Budget != MutationBudget {
				t.Fatal("incomplete acceptance contract", op)
			}
		} else if op.Responses["200"] == nil || route.Budget != BoundedBudget {
			t.Fatal("incomplete receipt contract", op)
		}
	}
}
