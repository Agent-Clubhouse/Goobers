package v30

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/goobers/goobers/api/schemas"
)

// schemaEnumAt walks a chain of object keys inside a decoded JSON Schema
// document and returns the string enum found at the end of the chain.
func schemaEnumAt(t *testing.T, raw []byte, keys ...string) []string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	node := any(doc)
	for _, key := range keys {
		m, ok := node.(map[string]any)
		if !ok {
			t.Fatalf("schema path %v: %q is not an object", keys, key)
		}
		node, ok = m[key]
		if !ok {
			t.Fatalf("schema path %v: missing key %q", keys, key)
		}
	}
	raw2, ok := node.([]any)
	if !ok {
		t.Fatalf("schema path %v: enum is not an array", keys)
	}
	values := make([]string, len(raw2))
	for i, v := range raw2 {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("schema path %v: enum entry %v is not a string", keys, v)
		}
		values[i] = s
	}
	return values
}

// TestPolicyActionSchemaEnumsMatchContracts is the ADO-N23 parity test: every
// policyActions/conditionalPolicyActions enum published in the author-facing
// JSON Schemas must equal the DSL 2.0 interpreter's known policy-action
// vocabulary exactly, so an action can never be declarable in a compiled
// contract without being acceptable to the schema (or vice versa).
func TestPolicyActionSchemaEnumsMatchContracts(t *testing.T) {
	known := append([]string(nil), knownPolicyActions()...)
	sort.Strings(known)

	workflowRaw, err := schemas.FS.ReadFile("workflow.schema.json")
	if err != nil {
		t.Fatalf("read workflow.schema.json: %v", err)
	}
	gooberRaw, err := schemas.FS.ReadFile("goober.schema.json")
	if err != nil {
		t.Fatalf("read goober.schema.json: %v", err)
	}

	cases := []struct {
		name string
		keys []string
		raw  []byte
	}{
		{
			name: "workflow.schema.json tasks[].policyActions.items.enum",
			keys: []string{"$defs", "task", "properties", "policyActions", "items", "enum"},
			raw:  workflowRaw,
		},
		{
			name: "goober.schema.json policyActions.items.enum",
			keys: []string{"properties", "spec", "properties", "policyActions", "items", "enum"},
			raw:  gooberRaw,
		},
		{
			name: "goober.schema.json conditionalPolicyActions.items.enum",
			keys: []string{"properties", "spec", "properties", "conditionalPolicyActions", "items", "enum"},
			raw:  gooberRaw,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := schemaEnumAt(t, tc.raw, tc.keys...)
			sorted := append([]string(nil), got...)
			sort.Strings(sorted)
			if !reflect.DeepEqual(sorted, known) {
				t.Fatalf("%s (sorted) = %v, want knownPolicyActions() = %v", tc.name, sorted, known)
			}
		})
	}
}
