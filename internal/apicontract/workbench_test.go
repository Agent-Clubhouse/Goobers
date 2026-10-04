package apicontract

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/goobers/goobers/internal/workbench"
)

func TestWorkbenchReadWireContract(t *testing.T) {
	item := workbench.BacklogItem{Ref: workbench.NodeRef{GaggleID: "team", SourceBindingID: "backlog", Kind: "work-item", SourceID: "123456"}, Locator: workbench.SourceLocator{ID: "42", URL: "https://github.com/acme/issues/issues/42"}, Revision: "2026-10-04T12:00:00Z", RevisionSemantics: "timestamp-preflight", Type: "Issue", Title: "Reliable workers", State: "open", Objective: true, RelationshipCoverage: workbench.RelationshipCoverage{Parents: "not-loaded", Blockers: "not-loaded", Milestones: "complete"}}
	page := workbench.BacklogPage{Items: []workbench.BacklogItem{item}, Candidates: 1, Exhausted: true, SourceTargetDigest: strings.Repeat("a", 64)}
	raw, err := json.Marshal(map[string]any{"components": map[string]any{"schemas": openAPIWorkbenchSchemas()}})
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]any{"BacklogItem": item, "BacklogPage": page, "BacklogPageRequest": workbench.BacklogPageRequest{Limit: 50}, "BacklogItemRequest": workbench.BacklogItemRequest{ID: "42", ExpectedSourceID: "123456"}} {
		compiler := jsonschema.NewCompiler()
		if err = compiler.AddResource("workbench.json", bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		schema, err := compiler.Compile("workbench.json#/components/schemas/" + name)
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
		decoded.(map[string]any)["credentialOverride"] = "automation"
		if err = schema.Validate(decoded); err == nil {
			t.Fatal(name, "accepts extra field")
		}
	}
}
