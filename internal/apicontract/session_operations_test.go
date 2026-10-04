package apicontract

import (
	"encoding/json"
	"testing"

	"github.com/goobers/goobers/internal/sessioning"
)

func TestSessionOperationContractIsClosedAndGrantScoped(t *testing.T) {
	data, err := OpenAPIDocument(false)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err = json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	paths := doc["paths"].(map[string]any)
	for _, name := range []string{"get_backlog_item", "list_backlog_items", "get_backlog_edit_capabilities", "edit_backlog_item", "get_backlog_edit_receipt"} {
		operation := paths[sessioning.OperationPath+"/"+name].(map[string]any)["post"].(map[string]any)
		if operation["x-goobers-session-grant"] != "live-turn" || len(operation["security"].([]any)) != 1 {
			t.Fatal("operation lacks turn-only authority")
		}
		schema := operation["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
		if schema["additionalProperties"] != false {
			t.Fatal("tool request is open")
		}
		properties := schema["properties"].(map[string]any)
		for _, field := range []string{"actor", "gaggle", "runId", "endpoint", "credential", "repository"} {
			if _, ok := properties[field]; ok {
				t.Fatal("authored authority", field)
			}
		}
	}
}
