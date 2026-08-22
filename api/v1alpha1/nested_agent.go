package v1alpha1

import (
	"fmt"
	"slices"
)

// NestedAgentPolicyVersion is the portable nested-agent policy contract.
const NestedAgentPolicyVersion = "v1alpha1"

// +kubebuilder:validation:Enum=disabled;coordinator-only;bounded
type DelegationAuthority string

const (
	DelegationDisabled    DelegationAuthority = "disabled"
	DelegationCoordinator DelegationAuthority = "coordinator-only"
	DelegationBounded     DelegationAuthority = "bounded"
)

// +kubebuilder:validation:Enum=fresh;inherited;explicit
type ContextMode string

const (
	ContextFresh     ContextMode = "fresh"
	ContextInherited ContextMode = "inherited"
	ContextExplicit  ContextMode = "explicit"
)

// +kubebuilder:validation:Enum=minimal;low;medium;high
type ReasoningEffort string

const (
	ReasoningMinimal ReasoningEffort = "minimal"
	ReasoningLow     ReasoningEffort = "low"
	ReasoningMedium  ReasoningEffort = "medium"
	ReasoningHigh    ReasoningEffort = "high"
)

// NestedAgentPolicy describes authority granted to children of an agentic stage.
// Empty policy means the stage is not using nested agents.
type NestedAgentPolicy struct {
	// +kubebuilder:validation:Enum=v1alpha1
	Version    string              `json:"version" yaml:"version"`
	Delegation DelegationAuthority `json:"delegation" yaml:"delegation"`
	// +kubebuilder:validation:Minimum=0
	MaxDepth int32 `json:"maxDepth,omitempty" yaml:"maxDepth,omitempty"`
	// +kubebuilder:validation:MinItems=1
	PermittedProfiles []string            `json:"permittedProfiles" yaml:"permittedProfiles"`
	Context           NestedContextPolicy `json:"context" yaml:"context"`
	Model             NestedModelPolicy   `json:"model" yaml:"model"`
	PeerMessaging     bool                `json:"peerMessaging,omitempty" yaml:"peerMessaging,omitempty"`
	PlatformPolicy    PlatformPolicy      `json:"platformPolicy" yaml:"platformPolicy"`
}

var supportedNestedAgentProfiles = map[string]struct{}{
	"coordinator": {},
	"reviewer":    {},
	"worker":      {},
}

type NestedContextPolicy struct {
	Mode             ContextMode `json:"mode" yaml:"mode"`
	ArtifactNames    []string    `json:"artifactNames,omitempty" yaml:"artifactNames,omitempty"`
	EnvelopeSections []string    `json:"envelopeSections,omitempty" yaml:"envelopeSections,omitempty"`
}

type NestedModelPolicy struct {
	Allowlist          []string        `json:"allowlist,omitempty" yaml:"allowlist,omitempty"`
	MaxReasoningEffort ReasoningEffort `json:"maxReasoningEffort,omitempty" yaml:"maxReasoningEffort,omitempty"`
}

// PlatformPolicy is immutable execution policy inherited by every child,
// including children using fresh context.
type PlatformPolicy struct {
	Capabilities  []string `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	PolicyActions []string `json:"policyActions,omitempty" yaml:"policyActions,omitempty"`
	Credentials   []string `json:"credentials,omitempty" yaml:"credentials,omitempty"`
	// +kubebuilder:validation:MinLength=1
	Sandbox           string   `json:"sandbox" yaml:"sandbox"`
	FilesystemRoots   []string `json:"filesystemRoots,omitempty" yaml:"filesystemRoots,omitempty"`
	NetworkEgress     []string `json:"networkEgress,omitempty" yaml:"networkEgress,omitempty"`
	ContentExclusions []string `json:"contentExclusions,omitempty" yaml:"contentExclusions,omitempty"`
	Budget            Limits   `json:"budget" yaml:"budget"`
	// +kubebuilder:validation:MinLength=1
	Cancellation string `json:"cancellation" yaml:"cancellation"`
	// +kubebuilder:validation:MinLength=1
	CompletionContract string `json:"completionContract" yaml:"completionContract"`
}

// ChildExecutionPolicy is the immutable policy handed to an admitted child.
type ChildExecutionPolicy struct {
	RunID          string              `json:"runId"`
	StageID        string              `json:"stageId"`
	Attempt        int32               `json:"attempt"`
	ParentAgent    string              `json:"parentAgent"`
	Profile        string              `json:"profile"`
	Objective      string              `json:"objective"`
	Ownership      string              `json:"ownership"`
	Capabilities   []string            `json:"capabilities,omitempty"`
	PolicyActions  []string            `json:"policyActions,omitempty"`
	PlatformPolicy PlatformPolicy      `json:"platformPolicy"`
	Delegation     DelegationAuthority `json:"delegation"`
	MaxDepth       int32               `json:"maxDepth,omitempty"`
	Context        NestedContextPolicy `json:"context"`
	Model          NestedModelPolicy   `json:"model"`
	PeerMessaging  bool                `json:"peerMessaging,omitempty"`
}

// StagePlatformAuthority derives the parent authority from runner-owned stage
// fields, never from the requested nested policy. Resource classes the runner
// cannot prove are deliberately absent so child admission fails closed.
func StagePlatformAuthority(env InvocationEnvelope, completionContract string) PlatformPolicy {
	roots := []string{"workspace"}
	for _, workspace := range env.AdditionalWorkspaces {
		roots = append(roots, "workspace:"+workspace.Name)
	}
	return PlatformPolicy{
		Capabilities:       append([]string(nil), env.Capabilities...),
		PolicyActions:      append([]string(nil), env.PolicyActions...),
		Credentials:        append([]string(nil), env.Capabilities...),
		Sandbox:            "workspace",
		FilesystemRoots:    roots,
		Budget:             env.Limits,
		Cancellation:       "stage-context",
		CompletionContract: completionContract,
	}
}

// Validate rejects unknown policy values and contradictory context/delegation
// settings before an adapter is allowed to launch a child.
func (p NestedAgentPolicy) Validate() error {
	if p.Version != NestedAgentPolicyVersion {
		return fmt.Errorf("nested agent policy: unsupported version %q", p.Version)
	}
	if len(p.PermittedProfiles) == 0 {
		return fmt.Errorf("nested agent policy: at least one permitted profile is required")
	}
	for _, profile := range p.PermittedProfiles {
		if _, ok := supportedNestedAgentProfiles[profile]; !ok {
			return fmt.Errorf("nested agent policy: unsupported profile %q", profile)
		}
	}
	switch p.Delegation {
	case DelegationDisabled:
		if p.MaxDepth != 0 {
			return fmt.Errorf("nested agent policy: disabled delegation cannot set maxDepth")
		}
	case DelegationCoordinator:
		if p.MaxDepth != 0 {
			return fmt.Errorf("nested agent policy: coordinator-only delegation cannot set maxDepth")
		}
	case DelegationBounded:
		if p.MaxDepth < 1 {
			return fmt.Errorf("nested agent policy: bounded delegation requires maxDepth >= 1")
		}
	default:
		return fmt.Errorf("nested agent policy: unsupported delegation %q", p.Delegation)
	}
	switch p.Context.Mode {
	case ContextFresh, ContextInherited:
		if len(p.Context.ArtifactNames) != 0 || len(p.Context.EnvelopeSections) != 0 {
			return fmt.Errorf("nested agent policy: context mode %q cannot select context", p.Context.Mode)
		}
	case ContextExplicit:
		if len(p.Context.ArtifactNames) == 0 && len(p.Context.EnvelopeSections) == 0 {
			return fmt.Errorf("nested agent policy: explicit context requires a selection")
		}
		for _, section := range p.Context.EnvelopeSections {
			if !slices.Contains(supportedEnvelopeSections, section) {
				return fmt.Errorf("nested agent policy: unsupported envelope section %q", section)
			}
		}
	default:
		return fmt.Errorf("nested agent policy: unsupported context mode %q", p.Context.Mode)
	}

	if p.Model.MaxReasoningEffort != "" && reasoningRank(string(p.Model.MaxReasoningEffort)) == 0 {
		return fmt.Errorf("nested agent policy: unsupported reasoning effort %q", p.Model.MaxReasoningEffort)
	}
	if p.PlatformPolicy.Sandbox == "" || p.PlatformPolicy.Cancellation == "" || p.PlatformPolicy.CompletionContract == "" {
		return fmt.Errorf("nested agent policy: platform policy must declare sandbox, cancellation, and completion contract")
	}
	if err := validateLimits(p.PlatformPolicy.Budget, "nested agent policy: child budget"); err != nil {
		return err
	}
	return nil
}

var supportedEnvelopeSections = []string{
	"run", "stage", "attempt", "parentAgent", "objective", "ownership",
	"capabilities", "policyActions",
	"platformPolicy", "completionContract", "cancellation", "budget",
}

// AdmitChild intersects parent authority with the requested policy and profile.
// It never widens capabilities, credentials, roots, egress, or model authority.
func AdmitChild(parent ChildExecutionPolicy, policy NestedAgentPolicy, profile, model, reasoning string) (ChildExecutionPolicy, error) {
	if err := policy.Validate(); err != nil {
		return ChildExecutionPolicy{}, err
	}
	if err := validateParentExecutionPolicy(parent); err != nil {
		return ChildExecutionPolicy{}, err
	}
	if !slices.Contains(policy.PermittedProfiles, profile) {
		return ChildExecutionPolicy{}, fmt.Errorf("nested agent policy: profile %q is not permitted", profile)
	}
	if len(policy.Model.Allowlist) > 0 && !slices.Contains(policy.Model.Allowlist, model) {
		return ChildExecutionPolicy{}, fmt.Errorf("nested agent policy: model %q is not permitted", model)
	}
	if len(parent.Model.Allowlist) > 0 && !slices.Contains(parent.Model.Allowlist, model) {
		return ChildExecutionPolicy{}, fmt.Errorf("nested agent policy: model %q exceeds parent authority", model)
	}
	if model == "" && len(policy.Model.Allowlist) > 0 {
		return ChildExecutionPolicy{}, fmt.Errorf("nested agent policy: a model is required when an allowlist is declared")
	}
	if model == "" && len(parent.Model.Allowlist) > 0 {
		return ChildExecutionPolicy{}, fmt.Errorf("nested agent policy: a model is required by parent authority")
	}
	if reasoning != "" && reasoningRank(reasoning) == 0 {
		return ChildExecutionPolicy{}, fmt.Errorf("nested agent policy: unsupported reasoning effort %q", reasoning)
	}
	if !isDelegationAllowed(parent.Delegation, parent.MaxDepth, policy.Delegation, policy.MaxDepth) {
		return ChildExecutionPolicy{}, fmt.Errorf("nested agent policy: child delegation exceeds parent authority")
	}
	if !isSubset(policy.PlatformPolicy.Capabilities, parent.PlatformPolicy.Capabilities) ||
		!isSubset(policy.PlatformPolicy.PolicyActions, parent.PlatformPolicy.PolicyActions) ||
		!isSubset(policy.PlatformPolicy.Credentials, parent.PlatformPolicy.Credentials) ||
		!isSubset(policy.PlatformPolicy.FilesystemRoots, parent.PlatformPolicy.FilesystemRoots) ||
		!isSubset(policy.PlatformPolicy.NetworkEgress, parent.PlatformPolicy.NetworkEgress) {
		return ChildExecutionPolicy{}, fmt.Errorf("nested agent policy: platform grants exceed parent authority")
	}
	if policy.PlatformPolicy.Sandbox != "" && policy.PlatformPolicy.Sandbox != parent.PlatformPolicy.Sandbox {
		return ChildExecutionPolicy{}, fmt.Errorf("nested agent policy: sandbox differs from parent authority")
	}
	if policy.PlatformPolicy.Cancellation != "" && policy.PlatformPolicy.Cancellation != parent.PlatformPolicy.Cancellation {
		return ChildExecutionPolicy{}, fmt.Errorf("nested agent policy: cancellation channel differs from parent authority")
	}
	if policy.PlatformPolicy.CompletionContract != "" && policy.PlatformPolicy.CompletionContract != parent.PlatformPolicy.CompletionContract {
		return ChildExecutionPolicy{}, fmt.Errorf("nested agent policy: completion contract differs from parent authority")
	}
	if !limitsWithin(policy.PlatformPolicy.Budget, parent.PlatformPolicy.Budget) {
		return ChildExecutionPolicy{}, fmt.Errorf("nested agent policy: budget exceeds parent authority")
	}
	if policy.Model.MaxReasoningEffort != "" && reasoning != "" && reasoningRank(reasoning) > reasoningRank(string(policy.Model.MaxReasoningEffort)) {
		return ChildExecutionPolicy{}, fmt.Errorf("nested agent policy: reasoning effort %q exceeds ceiling %q", reasoning, policy.Model.MaxReasoningEffort)
	}
	if parent.Model.MaxReasoningEffort != "" && reasoning != "" && reasoningRank(reasoning) > reasoningRank(string(parent.Model.MaxReasoningEffort)) {
		return ChildExecutionPolicy{}, fmt.Errorf("nested agent policy: reasoning effort %q exceeds parent ceiling %q", reasoning, parent.Model.MaxReasoningEffort)
	}
	child := parent
	child.Profile = profile
	child.PolicyActions = intersection(parent.PolicyActions, policy.PlatformPolicy.PolicyActions)
	child.Capabilities = intersection(parent.Capabilities, policy.PlatformPolicy.Capabilities)
	child.PlatformPolicy = intersectPlatform(parent.PlatformPolicy, policy.PlatformPolicy)
	child.Delegation = policy.Delegation
	child.MaxDepth = remainingDepth(parent, policy)
	child.Context = policy.Context
	child.Model = intersectModel(parent.Model, policy.Model)
	if len(child.Model.Allowlist) == 0 && len(parent.Model.Allowlist) > 0 {
		return ChildExecutionPolicy{}, fmt.Errorf("nested agent policy: child model authority has no intersection with parent")
	}
	child.PeerMessaging = parent.PeerMessaging && policy.PeerMessaging
	return child, nil
}

func validateParentExecutionPolicy(parent ChildExecutionPolicy) error {
	switch {
	case parent.RunID == "":
		return fmt.Errorf("nested agent policy: parent run identity is required")
	case parent.StageID == "":
		return fmt.Errorf("nested agent policy: parent stage identity is required")
	case parent.Attempt < 1:
		return fmt.Errorf("nested agent policy: parent attempt must be >= 1")
	case parent.ParentAgent == "":
		return fmt.Errorf("nested agent policy: parent agent identity is required")
	case parent.Objective == "":
		return fmt.Errorf("nested agent policy: parent objective is required")
	case parent.Ownership == "":
		return fmt.Errorf("nested agent policy: parent ownership boundary is required")
	case parent.PlatformPolicy.Sandbox == "":
		return fmt.Errorf("nested agent policy: parent sandbox authority is required")
	case parent.PlatformPolicy.Cancellation == "":
		return fmt.Errorf("nested agent policy: parent cancellation channel is required")
	case parent.PlatformPolicy.CompletionContract == "":
		return fmt.Errorf("nested agent policy: parent completion contract is required")
	}
	if !isSubset(parent.Capabilities, parent.PlatformPolicy.Capabilities) {
		return fmt.Errorf("nested agent policy: parent capabilities exceed parent platform authority")
	}
	if !isSubset(parent.PolicyActions, parent.PlatformPolicy.PolicyActions) {
		return fmt.Errorf("nested agent policy: parent policy actions exceed parent platform authority")
	}
	if err := validateLimits(parent.PlatformPolicy.Budget, "nested agent policy: parent budget"); err != nil {
		return err
	}
	return nil
}

func validateLimits(limits Limits, name string) error {
	switch {
	case limits.MaxDurationSeconds < 0:
		return fmt.Errorf("%s maxDurationSeconds must be >= 0", name)
	case limits.MaxTokens < 0:
		return fmt.Errorf("%s maxTokens must be >= 0", name)
	case limits.MaxCostUSD < 0:
		return fmt.Errorf("%s maxCostUSD must be >= 0", name)
	default:
		return nil
	}
}

func limitsWithin(requested, parent Limits) bool {
	return limitWithin(int64(requested.MaxDurationSeconds), int64(parent.MaxDurationSeconds)) &&
		limitWithin(requested.MaxTokens, parent.MaxTokens) &&
		floatLimitWithin(requested.MaxCostUSD, parent.MaxCostUSD)
}

func limitWithin(requested, parent int64) bool {
	return requested == 0 || parent == 0 || requested <= parent
}

func floatLimitWithin(requested, parent float64) bool {
	return requested == 0 || parent == 0 || requested <= parent
}

func isDelegationAllowed(parent DelegationAuthority, parentDepth int32, child DelegationAuthority, childDepth int32) bool {
	switch parent {
	case DelegationDisabled, DelegationCoordinator:
		return child == DelegationDisabled
	case DelegationBounded:
		if child == DelegationDisabled || child == DelegationCoordinator {
			return true
		}
		return child == DelegationBounded && childDepth < parentDepth
	case "":
		return child == DelegationDisabled
	default:
		return false
	}
}

func remainingDepth(parent ChildExecutionPolicy, policy NestedAgentPolicy) int32 {
	if policy.Delegation != DelegationBounded {
		return 0
	}
	if parent.Delegation != DelegationBounded {
		return 0
	}
	return policy.MaxDepth
}

func intersectModel(parent, requested NestedModelPolicy) NestedModelPolicy {
	out := requested
	if len(parent.Allowlist) > 0 {
		if len(requested.Allowlist) == 0 {
			out.Allowlist = append([]string(nil), parent.Allowlist...)
		} else {
			out.Allowlist = intersection(parent.Allowlist, requested.Allowlist)
		}
	}
	if parent.MaxReasoningEffort != "" &&
		(out.MaxReasoningEffort == "" || reasoningRank(string(parent.MaxReasoningEffort)) < reasoningRank(string(out.MaxReasoningEffort))) {
		out.MaxReasoningEffort = parent.MaxReasoningEffort
	}
	return out
}

func intersection(a, b []string) []string {
	var out []string
	for _, value := range a {
		if slices.Contains(b, value) && !slices.Contains(out, value) {
			out = append(out, value)
		}
	}
	return out
}

func isSubset(values, set []string) bool {
	for _, value := range values {
		if !slices.Contains(set, value) {
			return false
		}
	}
	return true
}

func intersectPlatform(a, b PlatformPolicy) PlatformPolicy {
	out := a
	out.Capabilities = intersection(a.Capabilities, b.Capabilities)
	out.PolicyActions = intersection(a.PolicyActions, b.PolicyActions)
	out.Credentials = intersection(a.Credentials, b.Credentials)
	out.FilesystemRoots = intersection(a.FilesystemRoots, b.FilesystemRoots)
	out.NetworkEgress = intersection(a.NetworkEgress, b.NetworkEgress)
	out.ContentExclusions = union(a.ContentExclusions, b.ContentExclusions)
	if b.Sandbox != "" {
		out.Sandbox = b.Sandbox
	}
	if b.Cancellation != "" {
		out.Cancellation = b.Cancellation
	}
	if b.CompletionContract != "" {
		out.CompletionContract = b.CompletionContract
	}
	if b.Budget.MaxDurationSeconds > 0 && (out.Budget.MaxDurationSeconds == 0 || b.Budget.MaxDurationSeconds < out.Budget.MaxDurationSeconds) {
		out.Budget.MaxDurationSeconds = b.Budget.MaxDurationSeconds
	}
	if b.Budget.MaxTokens > 0 && (out.Budget.MaxTokens == 0 || b.Budget.MaxTokens < out.Budget.MaxTokens) {
		out.Budget.MaxTokens = b.Budget.MaxTokens
	}
	if b.Budget.MaxCostUSD > 0 && (out.Budget.MaxCostUSD == 0 || b.Budget.MaxCostUSD < out.Budget.MaxCostUSD) {
		out.Budget.MaxCostUSD = b.Budget.MaxCostUSD
	}
	return out
}

func union(a, b []string) []string {
	out := append([]string(nil), a...)
	for _, value := range b {
		if !slices.Contains(out, value) {
			out = append(out, value)
		}
	}
	return out
}

func reasoningRank(value string) int {
	switch value {
	case "minimal":
		return 1
	case "low":
		return 2
	case "medium":
		return 3
	case "high":
		return 4
	default:
		return 0
	}
}
