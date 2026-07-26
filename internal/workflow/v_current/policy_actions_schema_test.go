package vcurrent

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/goobers/goobers/api/schemas"
	"github.com/goobers/goobers/internal/capability"
)

func TestPolicyActionSchemaEnumsMatchCompilerVocabulary(t *testing.T) {
	want := knownPolicyActions()
	tests := []struct {
		name       string
		schemaName string
		path       []string
	}{
		{
			name:       "goober policy actions",
			schemaName: "goober.schema.json",
			path:       []string{"properties", "spec", "properties", "policyActions", "items", "enum"},
		},
		{
			name:       "goober conditional policy actions",
			schemaName: "goober.schema.json",
			path:       []string{"properties", "spec", "properties", "conditionalPolicyActions", "items", "enum"},
		},
		{
			name:       "workflow task policy actions",
			schemaName: "workflow.schema.json",
			path:       []string{"$defs", "task", "properties", "policyActions", "items", "enum"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := schemaStringArray(t, tc.schemaName, tc.path...)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("enum = %v, want canonical compiler vocabulary %v", got, want)
			}
		})
	}
}

func TestCapabilitySchemaEnumsMatchStageDeclarableRegistry(t *testing.T) {
	var want []string
	for _, candidate := range capability.All() {
		value := string(candidate)
		if capability.StageDeclarable(value) {
			want = append(want, value)
		}
	}
	tests := []struct {
		name       string
		schemaName string
		path       []string
	}{
		{
			name:       "goober capabilities",
			schemaName: "goober.schema.json",
			path:       []string{"properties", "spec", "properties", "capabilities", "items", "enum"},
		},
		{
			name:       "workflow task capabilities",
			schemaName: "workflow.schema.json",
			path:       []string{"$defs", "task", "properties", "capabilities", "items", "enum"},
		},
		{
			name:       "invocation capabilities",
			schemaName: "invocation.schema.json",
			path:       []string{"properties", "capabilities", "items", "enum"},
		},
		{
			name:       "MCP credential capability",
			schemaName: "goober.schema.json",
			path:       []string{"$defs", "mcpCredentialRef", "properties", "capability", "enum"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := schemaStringArray(t, tc.schemaName, tc.path...)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("enum = %v, want stage-declarable capability registry %v", got, want)
			}
		})
	}
}

func schemaStringArray(t *testing.T, schemaName string, path ...string) []string {
	t.Helper()

	data, err := schemas.FS.ReadFile(schemaName)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var current any
	if err := json.Unmarshal(data, &current); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	for _, component := range path {
		object, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("%s is not an object while resolving %v", component, path)
		}
		current, ok = object[component]
		if !ok {
			t.Fatalf("missing %s while resolving %v", component, path)
		}
	}

	values, ok := current.([]any)
	if !ok {
		t.Fatalf("%v is not an array", path)
	}
	result := make([]string, len(values))
	for i, value := range values {
		result[i], ok = value.(string)
		if !ok {
			t.Fatalf("%v[%d] is not a string", path, i)
		}
	}
	return result
}
