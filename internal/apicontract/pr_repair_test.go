package apicontract

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/goobers/goobers/internal/sessioning"
)

func TestSessionPRRepairResponseSchemasAreClosedAndRetainCustody(t *testing.T) {
	schemas := mergeSchemaProperties(openAPISessionSchemas(), openAPIPRRepairSchemas())
	raw, err := json.Marshal(map[string]any{"components": map[string]any{"schemas": schemas}})
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	digest := strings.Repeat("b", 64)
	target := sessioning.PRRepairTarget{SourceBindingID: "code", Repository: sessioning.RepairRepository{Provider: "github", Owner: "acme", Name: "code"}, RepositorySourceID: "77", ID: "42", SourceID: "99", ExpectedHeadSHA: head}
	values := map[string]any{
		"SessionPRRepairInspection": sessioning.PRRepairInspection{Target: target, HeadSHA: head, BaseSHA: head, Head: "fix", Base: "main", Open: true},
		"SessionPRRepairFile":       sessioning.PRRepairFile{Target: target, HeadSHA: head, Path: "file.txt", Present: true, BlobID: head, Mode: "100644", Content: "new"},
		"SessionPRRepairCommand":    sessioning.PRRepairCommandView{ID: "repair-" + strings.Repeat("c", 32), SourceBindingID: "code", State: "unknown", RequestDigest: digest, OperationDigest: digest, SelectedHeadSHA: head, ExpectedHeadSHA: head, RunID: strings.Repeat("d", 32), Actor: sessioning.Actor{Issuer: "https://issuer.example", Subject: "alice"}, AcceptedAt: time.Now(), Receipt: &sessioning.PRRepairReceipt{OperationDigest: digest, Outcome: "unknown", MutationAttempted: true}},
	}
	for name, value := range values {
		compiler := jsonschema.NewCompiler()
		if err = compiler.AddResource("repair.json", bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		schema, err := compiler.Compile("repair.json#/components/schemas/" + name)
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
		object["credential"] = "forbidden"
		if schema.Validate(object) == nil {
			t.Fatal("open response", name)
		}
	}
}
