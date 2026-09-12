package schemas

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestGaggleIsolationSchemaStatesCurrentDispatchLimitation keeps the required
// schema shape unchanged while preventing its prose from promising a boundary
// the active worker does not enforce (#4897).
func TestGaggleIsolationSchemaStatesCurrentDispatchLimitation(t *testing.T) {
	data, err := FS.ReadFile("gaggle.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	spec := schemaObject(t, schema, "properties", "spec")
	isolation := schemaObject(t, spec, "properties", "isolation")
	required, ok := isolation["required"].([]any)
	if !ok || len(required) != 1 || required[0] != "namespace" {
		t.Fatalf("isolation.required = %#v, want unchanged [namespace] shape", isolation["required"])
	}
	for name, phrases := range map[string][]string{
		"isolation":   {"active mode-3 worker does not route", "All gaggles loaded", "--dispatch-namespace", "does not enforce per-gaggle isolation"},
		"namespace":   {"currently ignored by active mode-3 dispatch", "All loaded gaggles share", "--dispatch-namespace", "does not enforce per-gaggle isolation"},
		"identityRef": {"active dispatcher does not consume"},
	} {
		field := isolation
		if name != "isolation" {
			field = schemaObject(t, isolation, "properties", name)
		}
		description, _ := field["description"].(string)
		for _, phrase := range phrases {
			if !strings.Contains(description, phrase) {
				t.Errorf("%s description = %q, want %q", name, description, phrase)
			}
		}
	}
	combined := isolation["description"].(string) + " " + schemaObject(t, isolation, "properties", "namespace")["description"].(string)
	for _, policyClaim := range []string{"safe only", "supported topology", "recommended future"} {
		if strings.Contains(strings.ToLower(combined), policyClaim) {
			t.Errorf("isolation descriptions contain policy claim %q: %q", policyClaim, combined)
		}
	}
}

func schemaObject(t *testing.T, root map[string]any, path ...string) map[string]any {
	t.Helper()
	current := root
	for _, component := range path {
		next, ok := current[component].(map[string]any)
		if !ok {
			t.Fatalf("schema path %s is not an object", strings.Join(path, "."))
		}
		current = next
	}
	return current
}
