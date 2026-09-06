package strictyaml

import (
	"strings"
	"testing"
)

func TestYAMLToJSONRejectsDuplicateTopLevelKey(t *testing.T) {
	raw := []byte("name: a\nname: b\n")
	if _, err := YAMLToJSON(raw); err == nil {
		t.Fatal("expected error for duplicate top-level key, got nil")
	} else if !strings.Contains(err.Error(), `duplicate key "name"`) {
		t.Fatalf("error = %v, want mention of duplicate key %q", err, "name")
	}
}

func TestYAMLToJSONRejectsDuplicateNestedKey(t *testing.T) {
	raw := []byte("spec:\n  timeout: 1\n  other: x\n  timeout: 2\n")
	if _, err := YAMLToJSON(raw); err == nil {
		t.Fatal("expected error for duplicate nested key, got nil")
	} else if !strings.Contains(err.Error(), `duplicate key "timeout"`) {
		t.Fatalf("error = %v, want mention of duplicate key %q", err, "timeout")
	}
}

func TestYAMLToJSONAcceptsDistinctKeys(t *testing.T) {
	raw := []byte("name: a\nspec:\n  timeout: 1\n  other: x\n")
	jb, err := YAMLToJSON(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(jb), `"name":"a"`) {
		t.Fatalf("json = %s, want name field preserved", jb)
	}
}

func TestYAMLToJSONAllowsSameKeyInSiblingMappings(t *testing.T) {
	raw := []byte("items:\n  - name: a\n  - name: b\n")
	if _, err := YAMLToJSON(raw); err != nil {
		t.Fatalf("unexpected error for same key in sibling mappings: %v", err)
	}
}

func TestYAMLToJSONLeavesSyntaxErrorsToCaller(t *testing.T) {
	raw := []byte("name: [unterminated\n")
	_, err := YAMLToJSON(raw)
	if err == nil {
		t.Fatal("expected an error for malformed YAML")
	}
	if strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("error = %v, want the underlying parse error, not a duplicate-key error", err)
	}
}

func TestUnmarshalRejectsDuplicateKey(t *testing.T) {
	raw := []byte("kind: Goober\nkind: Gaggle\n")
	var out struct {
		Kind string `json:"kind"`
	}
	if err := Unmarshal(raw, &out); err == nil {
		t.Fatal("expected error for duplicate key, got nil")
	}
}
