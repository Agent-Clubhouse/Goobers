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
