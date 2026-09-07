package validate

import (
	"encoding/json"
	"testing"

	"github.com/goobers/goobers/internal/investigation"
)

func completeInvestigationDraft(t *testing.T) map[string]any {
	t.Helper()
	data, err := json.Marshal(completeInvestigationEvidence())
	if err != nil {
		t.Fatal(err)
	}
	var draft map[string]any
	if err := json.Unmarshal(data, &draft); err != nil {
		t.Fatal(err)
	}
	ref := func(stage string) map[string]any { return map[string]any{"producerStage": stage, "name": "evidence"} }
	for _, field := range [][3]string{{"reproduction", "harness", "reproduce"}, {"reproduction", "baseline", "reproduce"}, {"diagnosis", "report", "instrument"}, {"fix", "report", "implement-fix"}, {"validation", "result", "validate-reproduction"}} {
		draft[field[0]].(map[string]any)[field[1]] = ref(field[2])
	}
	for _, list := range []any{draft["diagnosis"].(map[string]any)["evidence"], draft["attachments"]} {
		for _, entry := range list.([]any) {
			entry.(map[string]any)["artifact"] = ref("instrument")
		}
	}
	draft["schemaVersion"] = investigation.DraftSchemaVersion
	return draft
}

func TestInvestigationDraftSchema(t *testing.T) {
	v := newV(t)
	validate := func(draft map[string]any) error {
		data, err := json.Marshal(draft)
		if err != nil {
			t.Fatal(err)
		}
		return v.ValidateJSON("investigation-evidence-draft.schema.json", data)
	}
	if err := validate(completeInvestigationDraft(t)); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"schemaVersion", "subject", "environment", "reproduction", "diagnosis", "fix", "validation"} {
		draft := completeInvestigationDraft(t)
		delete(draft, key)
		if err := validate(draft); err == nil {
			t.Fatalf("missing %s accepted", key)
		}
	}
	for name, mutate := range map[string]func(map[string]any){
		"canonical version": func(d map[string]any) { d["schemaVersion"] = investigation.SchemaVersion },
		"unknown field":     func(d map[string]any) { d["unknown"] = true },
		"predicted pointer": func(d map[string]any) {
			d["fix"].(map[string]any)["report"] = completeArtifactPointer("artifacts/forged")
		},
		"null attachments": func(d map[string]any) { d["attachments"] = nil },
		"blank name":       func(d map[string]any) { d["fix"].(map[string]any)["report"].(map[string]any)["name"] = "" },
		"unknown producer": func(d map[string]any) {
			d["fix"].(map[string]any)["report"].(map[string]any)["producerStage"] = "other-run"
		},
		"extra reference field": func(d map[string]any) {
			d["fix"].(map[string]any)["report"].(map[string]any)["path"] = "artifacts/forged"
		},
	} {
		t.Run(name, func(t *testing.T) {
			draft := completeInvestigationDraft(t)
			mutate(draft)
			if err := validate(draft); err == nil {
				t.Fatal("invalid draft accepted")
			}
		})
	}
	draft := completeInvestigationDraft(t)
	delete(draft, "attachments")
	if err := validate(draft); err != nil {
		t.Fatalf("optional attachments required: %v", err)
	}
}
