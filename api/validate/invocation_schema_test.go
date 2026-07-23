package validate

import (
	"encoding/json"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestInvocationTriggerRefValidatesAgainstSchema(t *testing.T) {
	validator, err := New()
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(apiv1.InvocationEnvelope{
		TaskID:     "select-pr",
		WorkflowID: "merge-review",
		RunID:      "run-1",
		TriggerRef: "github-webhook:pull_request#42",
		Gaggle:     "goobers",
		Goal:       "select the delivered pull request",
		Workspace:  "/workspace",
		RepoRef: apiv1.RepoRef{
			Provider: apiv1.ProviderGitHub,
			Owner:    "acme",
			Name:     "web",
		},
		Limits: apiv1.Limits{},
	})
	if err != nil {
		t.Fatalf("marshal invocation: %v", err)
	}
	if err := validator.ValidateJSON("invocation.schema.json", envelope); err != nil {
		t.Fatalf("marshaled invocation with triggerRef should validate: %v", err)
	}
}
