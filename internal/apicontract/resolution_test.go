package apicontract

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/goobers/goobers/internal/workbench"
)

func TestNeedsHumanSchemasRetainAssessmentAndRejectAuthorityInjection(t *testing.T) {
	schemas := mergeSchemaProperties(openAPIWorkbenchSchemas(), openAPIWorkbenchWriteSchemas())
	schemas = mergeSchemaProperties(schemas, openAPIResolutionSchemas())
	raw, err := json.Marshal(map[string]any{"components": map[string]any{"schemas": schemas}})
	if err != nil {
		t.Fatal(err)
	}
	fixture := newWireFixtures()
	digest := strings.Repeat("a", 64)
	ref := workbench.NeedsHumanEvidenceRef{Kind: "current-human-message", ID: "message-one", Digest: digest}
	assessment := workbench.NeedsHumanResolutionRequest{ID: "42", SourceID: "987654", ExpectedRevision: "revision", ObservationDigest: digest, Basis: ref, Rationale: "The actual human answer resolves the decision.", Evidence: []workbench.NeedsHumanEvidenceRef{ref}}
	observation := workbench.NeedsHumanObservation{Digest: digest, HumanInstruction: &ref, Item: fixture.WorkbenchItem, MarkerPresent: true, Comments: []workbench.NeedsHumanComment{}, Dependencies: []workbench.NeedsHumanDependency{}, CommentsComplete: true, DependenciesComplete: true, LearnedRecordDigest: digest, LearnedDependencies: []workbench.NeedsHumanDependency{}, LearnedComplete: true, WaitReasons: []string{}}
	command := workbench.NeedsHumanResolutionCommand{Assessment: assessment, ID: "resolution-" + strings.Repeat("a", 32), Gaggle: "web", SourceBindingID: "items", Actor: workbench.CommandActor{Issuer: "https://issuer.example", Subject: "human"}, ItemID: "42", SourceID: "987654", State: "unknown", RequestDigest: digest, OperationDigest: digest, Origin: workbench.NeedsHumanResolutionOrigin{RunID: strings.Repeat("b", 32), SessionID: "session-one", TurnID: "turn-one", MessageID: "message-one", MessageDigest: digest, GooberDigest: "sha256:" + digest}, AcceptedAt: time.Now().UTC(), NextAction: "Inspect this receipt."}
	for name, value := range map[string]any{"NeedsHumanObservation": observation, "NeedsHumanResolutionCommand": command} {
		compiler := jsonschema.NewCompiler()
		if err = compiler.AddResource("resolution.json", bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		schema, err := compiler.Compile("resolution.json#/components/schemas/" + name)
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
			t.Fatal("open authority schema", name)
		}
	}
}
