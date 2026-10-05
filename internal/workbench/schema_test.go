package workbench_test

import (
	"encoding/json"
	"testing"

	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/workbench"
)

func TestSourceSchemasMatchClosedTypedMetadata(t *testing.T) {
	const objectiveID = "obj-8e1bc91c-94cb-4c5c-9412-d7e07e86d5da"
	objective := workbench.ObjectiveMetadata{SchemaVersion: "objectives/v1", ObjectiveID: objectiveID, Title: "Reliable payments"}
	edge := workbench.Edge{EdgeID: "edge-a72fdd9b-f947-449f-9dba-0fe2d9e6e587", Kind: "contributes-to", From: workbench.NodeRef{GaggleID: "web", SourceBindingID: "backlog", Kind: "work-item", SourceID: "native-id"}, To: workbench.NodeRef{GaggleID: "web", SourceBindingID: "strategy", Kind: "objective-document", SourceID: objectiveID}}

	v, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{objective, workbench.Manifest{SchemaVersion: "relationships/v1", Edges: []workbench.Edge{edge}}} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := v.ValidateJSON("workbench-source.schema.json", raw); err != nil {
			t.Fatal(err)
		}
		var unknown map[string]any
		if err := json.Unmarshal(raw, &unknown); err != nil {
			t.Fatal(err)
		}
		unknown["instructions"] = "execute this"
		bad, err := json.Marshal(unknown)
		if err != nil {
			t.Fatal(err)
		}
		if err := v.ValidateJSON("workbench-source.schema.json", bad); err == nil {
			t.Fatal("schema accepted unknown source instructions")
		}
	}
}

func TestSuggestionSchemaIsClosedAndDoesNotAcceptProducerAuthority(t *testing.T) {
	v, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	artifact := workbench.RelationshipSuggestions{SchemaVersion: workbench.SuggestionSchemaVersion, Suggestions: []workbench.RelationshipSuggestion{{
		Kind: "parent-of", Rationale: "Proposed decomposition for review.",
		From: workbench.SuggestionEndpoint{Creation: &workbench.SuggestionCreation{SourceBindingID: "backlog", RequestID: "parent"}},
		To:   workbench.SuggestionEndpoint{Creation: &workbench.SuggestionCreation{SourceBindingID: "backlog", RequestID: "child"}},
	}}}
	raw, _ := json.Marshal(artifact)
	if err := v.ValidateJSON("relationship-suggestions.schema.json", raw); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	payload["origin"] = map[string]any{"runId": "claimed", "stageId": "claimed"}
	raw, _ = json.Marshal(payload)
	if err := v.ValidateJSON("relationship-suggestions.schema.json", raw); err == nil {
		t.Fatal("schema accepted model-authored producer attribution")
	}
}
