package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGaggleBundleDefinitionRejectsNestedUnknownFields(t *testing.T) {
	var definition GaggleBundleDefinition
	err := json.Unmarshal([]byte(`{
		"gaggle": {
			"apiVersion": "goobers.dev/v1alpha1",
			"kind": "Gaggle",
			"metadata": {"name": "example"},
			"spec": {"project": {"provider": "github", "owner": "example", "name": "repo"}, "unexpected": true}
		},
		"workflows": [],
		"goobers": [],
		"repositories": []
	}`), &definition)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("Unmarshal error = %v, want nested unknown-field rejection", err)
	}
}
