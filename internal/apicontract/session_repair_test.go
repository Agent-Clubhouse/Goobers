package apicontract

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/goobers/goobers/internal/sessioning"
)

func TestSharedSessionRepairSelectionSchemaIsClosed(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"components": map[string]any{"schemas": openAPISessionSchemas()}})
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("session.json", bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("session.json#/components/schemas/SessionMessageRequest")
	if err != nil {
		t.Fatal(err)
	}
	req := SessionMessageRequest{Text: "Repair", RepairTarget: &sessioning.PRRepairTarget{SourceBindingID: "code", Repository: sessioning.RepairRepository{Provider: "github", Owner: "org", Name: "repo"}, RepositorySourceID: "100", ID: "12", SourceID: "900", ExpectedHeadSHA: strings.Repeat("a", 40)}}
	encoded, _ := json.Marshal(req)
	var decoded map[string]any
	if err = json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if err = schema.Validate(decoded); err != nil {
		t.Fatal(err)
	}
	target := decoded["repairTarget"].(map[string]any)
	target["actor"] = "other"
	if schema.Validate(decoded) == nil {
		t.Fatal("selection accepts actor authority")
	}
	delete(target, "actor")
	target["repository"].(map[string]any)["credentialRef"] = "automation"
	if schema.Validate(decoded) == nil {
		t.Fatal("repository accepts credential override")
	}
}
