package v1alpha1

import (
	"slices"
	"strings"
	"testing"
)

func validNestedPolicy() NestedAgentPolicy {
	return NestedAgentPolicy{
		Version: NestedAgentPolicyVersion, Delegation: DelegationDisabled,
		Context:           NestedContextPolicy{Mode: ContextFresh},
		PermittedProfiles: []string{"worker"},
		PlatformPolicy: PlatformPolicy{
			Capabilities: []string{"repo:read"}, Credentials: []string{"repo:read"},
			Sandbox: "workspace", Cancellation: "run", CompletionContract: "result",
		},
	}
}

func validParent() ChildExecutionPolicy {
	return ChildExecutionPolicy{
		RunID: "run-1", StageID: "run-1:parent", Attempt: 1,
		ParentAgent: "parent", Objective: "delegate work", Ownership: "task:parent",
		PlatformPolicy: PlatformPolicy{
			Capabilities: []string{"repo:read", "repo:push"}, PolicyActions: []string{"modify-repository"},
			Credentials:     []string{"repo:read", "repo:push"},
			FilesystemRoots: []string{"workspace"}, NetworkEgress: []string{"github"},
			ContentExclusions: []string{"secrets"}, Sandbox: "workspace",
			Cancellation: "run", CompletionContract: "result",
		},
		Capabilities: []string{"repo:read", "repo:push"}, PolicyActions: []string{"modify-repository"},
		PeerMessaging: true,
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

func TestAdmitChildIntersectsPolicyActions(t *testing.T) {
	parent := validParent()
	policy := validNestedPolicy()
	policy.PlatformPolicy.PolicyActions = []string{"modify-repository"}

	child, err := AdmitChild(parent, policy, "worker", "", "")
	if err != nil {
		t.Fatalf("AdmitChild() error = %v", err)
	}
	if len(child.PolicyActions) != 1 || child.PolicyActions[0] != "modify-repository" {
		t.Fatalf("policy actions = %v, want [modify-repository]", child.PolicyActions)
	}
}

func TestAdmitChildRejectsResourceWidening(t *testing.T) {
	tests := []struct {
		name string
		edit func(*NestedAgentPolicy)
	}{
		{"capability", func(p *NestedAgentPolicy) {
			p.PlatformPolicy.Capabilities = append(p.PlatformPolicy.Capabilities, "admin")
		}},
		{"policy action", func(p *NestedAgentPolicy) { p.PlatformPolicy.PolicyActions = []string{"approve-release"} }},
		{"credential", func(p *NestedAgentPolicy) { p.PlatformPolicy.Credentials = []string{"admin-token"} }},
		{"filesystem", func(p *NestedAgentPolicy) { p.PlatformPolicy.FilesystemRoots = []string{"host-root"} }},
		{"egress", func(p *NestedAgentPolicy) { p.PlatformPolicy.NetworkEgress = []string{"internet"} }},
		{"sandbox", func(p *NestedAgentPolicy) { p.PlatformPolicy.Sandbox = "host" }},
		{"cancellation", func(p *NestedAgentPolicy) { p.PlatformPolicy.Cancellation = "none" }},
		{"completion", func(p *NestedAgentPolicy) { p.PlatformPolicy.CompletionContract = "optional" }},
		{"duration budget", func(p *NestedAgentPolicy) { p.PlatformPolicy.Budget.MaxDurationSeconds = 121 }},
		{"token budget", func(p *NestedAgentPolicy) { p.PlatformPolicy.Budget.MaxTokens = 1001 }},
		{"cost budget", func(p *NestedAgentPolicy) { p.PlatformPolicy.Budget.MaxCostUSD = 11 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parent := validParent()
			parent.PlatformPolicy.Budget = Limits{
				MaxDurationSeconds: 120,
				MaxTokens:          1000,
				MaxCostUSD:         10,
			}
			policy := validNestedPolicy()
			test.edit(&policy)
			if _, err := AdmitChild(parent, policy, "worker", "", ""); err == nil {
				t.Fatal("resource widening was admitted")
			}
		})
	}
}

func TestAdmitChildInheritsParentBudgetWhenChildOmitsLimit(t *testing.T) {
	parent := validParent()
	parent.PlatformPolicy.Budget = Limits{
		MaxDurationSeconds: 120,
		MaxTokens:          1000,
		MaxCostUSD:         10,
	}
	child, err := AdmitChild(parent, validNestedPolicy(), "worker", "", "")
	if err != nil {
		t.Fatalf("AdmitChild() error = %v", err)
	}
	if child.PlatformPolicy.Budget != parent.PlatformPolicy.Budget {
		t.Fatalf("budget = %+v, want inherited %+v", child.PlatformPolicy.Budget, parent.PlatformPolicy.Budget)
	}
}

func TestAdmitChildRequiresRunnerOwnedParentAuthority(t *testing.T) {
	tests := []struct {
		name string
		edit func(*ChildExecutionPolicy)
	}{
		{"run", func(p *ChildExecutionPolicy) { p.RunID = "" }},
		{"stage", func(p *ChildExecutionPolicy) { p.StageID = "" }},
		{"attempt", func(p *ChildExecutionPolicy) { p.Attempt = 0 }},
		{"agent", func(p *ChildExecutionPolicy) { p.ParentAgent = "" }},
		{"objective", func(p *ChildExecutionPolicy) { p.Objective = "" }},
		{"ownership", func(p *ChildExecutionPolicy) { p.Ownership = "" }},
		{"sandbox", func(p *ChildExecutionPolicy) { p.PlatformPolicy.Sandbox = "" }},
		{"cancellation", func(p *ChildExecutionPolicy) { p.PlatformPolicy.Cancellation = "" }},
		{"completion", func(p *ChildExecutionPolicy) { p.PlatformPolicy.CompletionContract = "" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parent := validParent()
			test.edit(&parent)
			if _, err := AdmitChild(parent, validNestedPolicy(), "worker", "", ""); err == nil {
				t.Fatal("child was admitted without complete parent authority")
			}
		})
	}
}

func TestStagePlatformAuthorityDoesNotTrustNestedRequest(t *testing.T) {
	env := InvocationEnvelope{
		Capabilities:  []string{"repo:read"},
		PolicyActions: []string{"modify-repository"},
		Limits:        Limits{MaxDurationSeconds: 60},
		AdditionalWorkspaces: []AdditionalWorkspace{
			{Name: "docs", Path: "ignored"},
		},
		NestedAgentPolicy: &NestedAgentPolicy{
			PlatformPolicy: PlatformPolicy{
				Capabilities:  []string{"admin"},
				PolicyActions: []string{"approve-release"},
				Credentials:   []string{"root-token"},
				NetworkEgress: []string{"internet"},
			},
		},
	}
	authority := StagePlatformAuthority(env, "result")
	if !slices.Equal(authority.Capabilities, []string{"repo:read"}) ||
		!slices.Equal(authority.PolicyActions, []string{"modify-repository"}) ||
		!slices.Equal(authority.Credentials, []string{"repo:read"}) ||
		!slices.Equal(authority.FilesystemRoots, []string{"workspace", "workspace:docs"}) ||
		len(authority.NetworkEgress) != 0 ||
		authority.Budget != env.Limits {
		t.Fatalf("authority = %+v, want runner-owned stage fields only", authority)
	}
}
