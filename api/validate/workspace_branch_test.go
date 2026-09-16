package validate

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOwnedBranchResultSchemaIsClosed(t *testing.T) {
	validator, err := New()
	if err != nil {
		t.Fatal(err)
	}
	valid := `{"status":"success","workspaceBranchBinding":{"repository":{"provider":"github","owner":"acme","name":"base"},"ref":"refs/heads/ns/workflow/run","startingSha":"` + strings.Repeat("a", 40) + `"}}`
	for _, tc := range []struct {
		name, data string
		valid      bool
	}{
		{"valid", valid, true},
		{"null", `{"status":"success","workspaceBranchBinding":null}`, false},
		{"scalar", `{"status":"success","workspaceBranchBinding":"main"}`, false},
		{"unknown", strings.Replace(valid, `"startingSha":`, `"credentials":"forged","startingSha":`, 1), false},
		{"short-sha", strings.Replace(valid, strings.Repeat("a", 40), "abc", 1), false},
		{"not-a-branch", strings.Replace(valid, "refs/heads/ns/workflow/run", "main", 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validator.ValidateEnvelope("result", json.RawMessage(tc.data)); (err == nil) != tc.valid {
				t.Fatalf("valid = %t, error = %v", tc.valid, err)
			}
		})
	}
}
