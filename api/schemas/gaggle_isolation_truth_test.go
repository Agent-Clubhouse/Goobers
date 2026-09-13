package schemas

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestGaggleIsolationSchemaStatesCurrentDispatchBehavior keeps the required
// schema shape unchanged while preventing its prose from drifting from what
// the active worker actually does: namespace now IS routed and enforced by
// the mode-3 dispatch path (#4897); identityRef is not yet consumed.
func TestGaggleIsolationSchemaStatesCurrentDispatchBehavior(t *testing.T) {
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
		"isolation":   {"routes every stage pod", "declared namespace", "#4897"},
		"namespace":   {"dispatcher routes each stage pod here", "refuses to start", "#4897"},
		"identityRef": {"does not yet consume"},
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
	for _, staleClaim := range []string{"does not route", "does not enforce per-gaggle isolation", "all gaggles loaded", "all loaded gaggles share"} {
		if strings.Contains(strings.ToLower(combined), staleClaim) {
			t.Errorf("isolation descriptions still contain the pre-#4897 limitation claim %q: %q", staleClaim, combined)
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
