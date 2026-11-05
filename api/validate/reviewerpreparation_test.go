package validate

import (
	"encoding/json"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestInvocationPreparedReviewerDiffClosedSchema(t *testing.T) {
	validator, err := New()
	if err != nil {
		t.Fatal(err)
	}
	envelope := apiv1.InvocationEnvelope{TaskID: "run:review", WorkflowID: "review", RunID: "run", Gaggle: "web", Goal: "review", Workspace: "/workspace", RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}}
	data, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		value any
		valid bool
	}{
		{"legacy", nil, true},
		{"observed empty", map[string]any{"observed": true}, true},
		{"unobserved", map[string]any{"observed": false}, true},
		{"missing observation", map[string]any{}, false},
		{"unknown field", map[string]any{"observed": true, "invented": true}, false},
		{"recorded", map[string]any{"observed": true, "artifact": map[string]any{"path": "artifacts/diff.patch", "digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}, true},
		{"unobserved artifact", map[string]any{"observed": false, "artifact": map[string]any{"path": "artifacts/diff.patch", "digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			delete(wire, "preparedReviewerDiff")
			if tc.value != nil {
				wire["preparedReviewerDiff"] = tc.value
			}
			data, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			err = validator.ValidateJSON("invocation.schema.json", data)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}
