package v1alpha1

import (
	"strings"
	"testing"
)

func validNestedPolicy() NestedAgentPolicy {
	return NestedAgentPolicy{
		Version: NestedAgentPolicyVersion, Delegation: DelegationDisabled,
		Context:           NestedContextPolicy{Mode: ContextFresh},
		PermittedProfiles: []string{"worker"},
		PlatformPolicy: PlatformPolicy{
			Capabilities: []string{"repo:read"}, Credentials: []string{"repo-token"},
			Sandbox: "workspace", Cancellation: "run", CompletionContract: "result",
		},
	}
}

func validParent() ChildExecutionPolicy {
	return ChildExecutionPolicy{
		PlatformPolicy: PlatformPolicy{
			Capabilities: []string{"repo:read", "repo:push"}, Credentials: []string{"repo-token"},
			FilesystemRoots: []string{"workspace"}, NetworkEgress: []string{"github"},
			ContentExclusions: []string{"secrets"}, Sandbox: "workspace",
			Cancellation: "run", CompletionContract: "result",
		},
		Capabilities: []string{"repo:read", "repo:push"}, PeerMessaging: true,
	}
}

func TestNestedAgentPolicyValidation(t *testing.T) {
	tests := []struct {
		name string
		edit func(*NestedAgentPolicy)
		want string
	}{
		{"unknown context", func(p *NestedAgentPolicy) { p.Context.Mode = "history" }, "unsupported context"},
		{"bounded without depth", func(p *NestedAgentPolicy) { p.Delegation = DelegationBounded }, "maxDepth"},
		{"explicit without selection", func(p *NestedAgentPolicy) { p.Context.Mode = ContextExplicit }, "selection"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := validNestedPolicy()
			test.edit(&policy)
			if err := policy.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestAdmitChildIntersectsAuthorityAndRejectsWidening(t *testing.T) {
	parent := validParent()
	policy := validNestedPolicy()
	child, err := AdmitChild(parent, policy, "worker", "", "")
	if err != nil {
		t.Fatalf("AdmitChild() error = %v", err)
	}
	if len(child.Capabilities) != 1 || child.Capabilities[0] != "repo:read" {
		t.Fatalf("capabilities = %v, want [repo:read]", child.Capabilities)
	}
	if child.PeerMessaging {
		t.Fatal("peer messaging was widened by child policy")
	}

	policy.PlatformPolicy.Capabilities = []string{"admin"}
	if _, err := AdmitChild(parent, policy, "worker", "", ""); err == nil {
		t.Fatal("privilege widening was admitted")
	}
}

func TestAdmitChildPreservesParentCeilings(t *testing.T) {
	parent := validParent()
	parent.Delegation = DelegationBounded
	parent.MaxDepth = 2
	parent.Model = NestedModelPolicy{
		Allowlist:          []string{"safe-model", "fallback-model"},
		MaxReasoningEffort: ReasoningMedium,
	}

	policy := validNestedPolicy()
	policy.Delegation = DelegationBounded
	policy.MaxDepth = 3
	policy.Model = NestedModelPolicy{
		Allowlist:          []string{"safe-model", "unsafe-model"},
		MaxReasoningEffort: ReasoningHigh,
	}

	if _, err := AdmitChild(parent, policy, "worker", "unsafe-model", "high"); err == nil {
		t.Fatal("child widened parent model authority")
	}
	if _, err := AdmitChild(parent, policy, "worker", "safe-model", "high"); err == nil {
		t.Fatal("child widened parent reasoning authority")
	}
	policy.MaxDepth = 1
	child, err := AdmitChild(parent, policy, "worker", "safe-model", "medium")
	if err != nil {
		t.Fatalf("AdmitChild() error = %v", err)
	}
	if child.MaxDepth != 1 || len(child.Model.Allowlist) != 1 || child.Model.Allowlist[0] != "safe-model" ||
		child.Model.MaxReasoningEffort != ReasoningMedium {
		t.Fatalf("child authority = %+v, want parent-intersected authority", child)
	}
}

func TestAdmitChildPreservesContentExclusions(t *testing.T) {
	parent := validParent()
	policy := validNestedPolicy()
	policy.PlatformPolicy.ContentExclusions = nil
	child, err := AdmitChild(parent, policy, "worker", "", "")
	if err != nil {
		t.Fatalf("AdmitChild() error = %v", err)
	}
	if len(child.PlatformPolicy.ContentExclusions) != 1 || child.PlatformPolicy.ContentExclusions[0] != "secrets" {
		t.Fatalf("content exclusions = %v, want inherited parent exclusions", child.PlatformPolicy.ContentExclusions)
	}
}

func TestAdmitChildRejectsUnsupportedProfile(t *testing.T) {
	policy := validNestedPolicy()
	policy.PermittedProfiles = []string{"unknown"}
	if _, err := AdmitChild(validParent(), policy, "unknown", "", ""); err == nil {
		t.Fatal("unknown profile was admitted")
	}
}

func TestAdmitChildRejectsEmptyModelIntersection(t *testing.T) {
	parent := validParent()
	parent.Model.Allowlist = []string{"parent-model"}
	policy := validNestedPolicy()
	policy.Model.Allowlist = []string{"child-model"}

	if _, err := AdmitChild(parent, policy, "worker", "child-model", ""); err == nil {
		t.Fatal("child with no model intersection was admitted")
	}
}
