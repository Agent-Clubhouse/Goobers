package schemas

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

func TestVerdictDispositionContract(t *testing.T) {
	compiler := jsonschema.NewCompiler()
	for _, file := range Files() {
		raw, err := FS.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := compiler.AddResource(BaseURI+file, bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
	}
	schema, err := compiler.Compile(BaseURI + "verdict.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, raw string
		valid     bool
	}{
		{"legacy fail", `{"decision":"fail"}`, true},
		{"legacy pass", `{"decision":"pass"}`, true},
		{"legacy needs changes", `{"decision":"needs-changes"}`, true},
		{"ordering", `{"decision":"defer","reasonCode":"ordering","rationale":"Wait for sibling #9."}`, true},
		{"no lander", `{"decision":"defer","reasonCode":"no-lander","rationale":"No current candidate can land.","elected":false}`, true},
		{"implementation rejection", `{"decision":"fail","reasonCode":"implementation-rejected","rationale":"Approach violates the required contract."}`, true},
		{"policy rejection", `{"decision":"fail","reasonCode":"policy-rejected","rationale":"Requested mutation is forbidden."}`, true},
		{"missing reason", `{"decision":"defer","rationale":"Wait."}`, false},
		{"missing rationale", `{"decision":"defer","reasonCode":"ordering"}`, false},
		{"blank rationale", `{"decision":"defer","reasonCode":"ordering","rationale":" \n\t"}`, false},
		{"deferral cannot reject", `{"decision":"defer","reasonCode":"policy-rejected","rationale":"Wait."}`, false},
		{"rejection cannot order", `{"decision":"fail","reasonCode":"ordering","rationale":"Wait."}`, false},
		{"deferral cannot elect", `{"decision":"defer","reasonCode":"ordering","rationale":"Wait.","elected":true}`, false},
		{"pass cannot defer", `{"decision":"pass","reasonCode":"ordering","rationale":"Wait."}`, false},
		{"unknown code", `{"decision":"fail","reasonCode":"other","rationale":"Rejected."}`, false},
		{"typed rejection needs rationale", `{"decision":"fail","reasonCode":"implementation-rejected"}`, false},
		{"unknown decision", `{"decision":"skip"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var value any
			if err := json.Unmarshal([]byte(tc.raw), &value); err != nil {
				t.Fatal(err)
			}
			if err := schema.Validate(value); (err == nil) != tc.valid {
				t.Fatalf("valid=%t err=%v", tc.valid, err)
			}
		})
	}
}
