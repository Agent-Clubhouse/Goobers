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

// TestInvocationRepoRefKeepsCheckoutOffTheWire locks both halves of the
// accepted-but-inert checkout posture (B2, #649): an envelope built through
// RepoRef.EnvelopeRef validates even when the gaggle declares
// project.checkout, and the schema's closed repoRef stays the enforcement —
// checkout riding the wire is a contract violation, not a schema gap.
func TestInvocationRepoRefKeepsCheckoutOffTheWire(t *testing.T) {
	validator, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ref := apiv1.RepoRef{
		Provider: apiv1.ProviderGitHub,
		Owner:    "acme",
		Name:     "web",
		Checkout: &apiv1.CheckoutSpec{Sparse: []string{"services/web"}},
	}
	invocation := apiv1.InvocationEnvelope{
		TaskID:     "implement",
		WorkflowID: "issue-fix",
		RunID:      "run-1",
		Gaggle:     "acme-web",
		Goal:       "implement the fix",
		Workspace:  "/workspace",
		RepoRef:    ref.EnvelopeRef(),
		Limits:     apiv1.Limits{},
	}
	envelope, err := json.Marshal(invocation)
	if err != nil {
		t.Fatalf("marshal invocation: %v", err)
	}
	if err := validator.ValidateJSON("invocation.schema.json", envelope); err != nil {
		t.Fatalf("envelope from a checkout-declaring gaggle should validate: %v", err)
	}

	invocation.RepoRef = ref
	leaked, err := json.Marshal(invocation)
	if err != nil {
		t.Fatalf("marshal invocation: %v", err)
	}
	if err := validator.ValidateJSON("invocation.schema.json", leaked); err == nil {
		t.Fatal("checkout on the envelope repoRef should fail the closed schema")
	}
}
