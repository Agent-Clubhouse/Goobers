package apicontract

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

func TestSharedSessionWireAndClosedRequestSchemas(t *testing.T) {
	schemas := openAPISessionSchemas()
	raw, err := json.Marshal(map[string]any{"components": map[string]any{"schemas": schemas}})
	if err != nil {
		t.Fatal(err)
	}
	f := newWireFixtures()
	for name, value := range map[string]any{"SessionCreateRequest": f.SessionCreate, "SessionMessageRequest": f.SessionInput, "SessionCloseRequest": f.SessionClose, "InteractiveSession": f.Session, "SessionPage": f.Sessions, "SessionMessagePage": f.SessionMessages, "SessionAcceptance": f.SessionAccepted} {
		c := jsonschema.NewCompiler()
		if err = c.AddResource("session.json", bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		schema, err := c.Compile("session.json#/components/schemas/" + name)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(value)
		var decoded any
		if err = json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		if err = schema.Validate(decoded); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		object := decoded.(map[string]any)
		object["actorOverride"] = "other"
		if err = schema.Validate(object); err == nil {
			t.Fatalf("%s accepts extra authority", name)
		}
	}
}
func TestSharedSessionRoutesAlwaysRequireHumanAuthAndMutationKeys(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		raw, err := OpenAPIDocument(authenticated)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Paths map[string]map[string]struct {
				Security   []any `json:"security"`
				Parameters []struct {
					Name     string `json:"name"`
					Required bool   `json:"required"`
				} `json:"parameters"`
				Responses map[string]any `json:"responses"`
			} `json:"paths"`
		}
		if err = json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		for _, route := range V1Routes() {
			if !sessionRoute(route.ID) {
				continue
			}
			method := "get"
			if route.Method == http.MethodPost {
				method = "post"
			}
			op := doc.Paths[route.Path][method]
			if len(op.Security) != 1 {
				t.Fatal(route.ID, "missing auth")
			}
			key := false
			for _, p := range op.Parameters {
				if p.Name == "Idempotency-Key" {
					key = p.Required
				}
			}
			if key != (method == "post") {
				t.Fatal(route.ID, "wrong key contract")
			}
			if method == "post" && (op.Responses["202"] == nil || op.Responses["204"] != nil || op.Responses["200"] != nil) {
				t.Fatal(route.ID, "wrong acceptance response")
			}
		}
	}
}
