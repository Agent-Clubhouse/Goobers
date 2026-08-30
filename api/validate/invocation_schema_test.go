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

func TestInvocationBranchArtifactValidatesAgainstSchema(t *testing.T) {
	validator, err := New()
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(apiv1.InvocationEnvelope{
		TaskID: "collate", WorkflowID: "quality-sprint", RunID: "run-1",
		Gaggle: "goobers", Goal: "collate branch findings", Workspace: "/workspace",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
		Limits:  apiv1.Limits{},
		ContextPointers: []apiv1.ContextPointer{{
			Name: "review-security.artifact[0]", Branch: 1, BranchName: "security",
			Artifact: &apiv1.ArtifactPointer{Path: "artifacts/security.md", Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		}},
	})
	if err != nil {
		t.Fatalf("marshal invocation: %v", err)
	}
	if err := validator.ValidateJSON("invocation.schema.json", envelope); err != nil {
		t.Fatalf("branch-attributed artifact invocation should validate: %v", err)
	}

	var value map[string]any
	if err := json.Unmarshal(envelope, &value); err != nil {
		t.Fatal(err)
	}
	pointers := value["contextPointers"].([]any)
	delete(pointers[0].(map[string]any), "branchName")
	invalid, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := validator.ValidateJSON("invocation.schema.json", invalid); err == nil {
		t.Fatal("branch attribution without branchName should fail the closed schema")
	}
}

func TestNestedInvocationRequiresRunnerAuthorityFields(t *testing.T) {
	validator, err := New()
	if err != nil {
		t.Fatal(err)
	}
	invocation := apiv1.InvocationEnvelope{
		TaskID:            "implement",
		Attempt:           1,
		WorkflowID:        "issue-fix",
		RunID:             "run-1",
		Gaggle:            "acme-web",
		Goober:            "coder",
		Goal:              "implement the fix",
		OwnershipBoundary: "task:implement",
		Workspace:         "/workspace",
		RepoRef:           apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
		Capabilities:      []string{"repo:read"},
		PolicyActions:     []string{"modify-repository"},
		Limits:            apiv1.Limits{MaxDurationSeconds: 60},
		NestedAgentPolicy: &apiv1.NestedAgentPolicy{
			Version:           apiv1.NestedAgentPolicyVersion,
			Delegation:        apiv1.DelegationDisabled,
			PermittedProfiles: []string{"worker"},
			Context:           apiv1.NestedContextPolicy{Mode: apiv1.ContextFresh},
			PlatformPolicy: apiv1.PlatformPolicy{
				Capabilities:       []string{"repo:read"},
				PolicyActions:      []string{"modify-repository"},
				Credentials:        []string{"repo:read"},
				Sandbox:            "workspace",
				FilesystemRoots:    []string{"workspace"},
				Cancellation:       "stage-context",
				CompletionContract: "result",
			},
		},
	}
	parent := apiv1.StagePlatformAuthority(invocation, "result")
	invocation.ParentPlatformPolicy = &parent
	envelope, err := json.Marshal(invocation)
	if err != nil {
		t.Fatalf("marshal invocation: %v", err)
	}
	if err := validator.ValidateJSON("invocation.schema.json", envelope); err != nil {
		t.Fatalf("complete nested invocation should validate: %v", err)
	}

	var value map[string]any
	if err := json.Unmarshal(envelope, &value); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"attempt", "goober", "ownershipBoundary", "parentPlatformPolicy"} {
		invalid := make(map[string]any, len(value))
		for name, entry := range value {
			invalid[name] = entry
		}
		delete(invalid, field)
		payload, err := json.Marshal(invalid)
		if err != nil {
			t.Fatal(err)
		}
		if err := validator.ValidateJSON("invocation.schema.json", payload); err == nil {
			t.Fatalf("nested invocation without %s should fail the closed schema", field)
		}
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
