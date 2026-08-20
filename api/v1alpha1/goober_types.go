package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Harness identifies the agent harness a goober runs on.
type Harness string

const (
	// HarnessCopilot is the GitHub Copilot agent harness (v1 default).
	HarnessCopilot Harness = "copilot"
	// HarnessClaudeCode is the Anthropic Claude Code agent harness.
	HarnessClaudeCode Harness = "claude-code"
)

// MCPHeaderScheme controls how a resolved credential is formatted in a remote
// MCP server header.
type MCPHeaderScheme string

const (
	// MCPHeaderSchemeBearer prefixes a remote header credential with "Bearer ".
	MCPHeaderSchemeBearer MCPHeaderScheme = "bearer"
	// MCPHeaderSchemeBasic prefixes a remote header credential with "Basic ".
	MCPHeaderSchemeBasic MCPHeaderScheme = "basic"
)

// MCPCredentialKind identifies a non-capability MCP credential source.
type MCPCredentialKind string

const (
	// MCPCredentialKindBYO resolves a named operator-provided credential.
	MCPCredentialKindBYO MCPCredentialKind = "byo"
)

// MCPCredentialRef binds either one first-party capability credential or one
// named BYO credential to an MCP server environment variable or HTTP header.
// Secret values remain in instance credential configuration and are resolved
// only for the current invocation.
// +kubebuilder:validation:XValidation:rule="(has(self.capability) && !has(self.kind) && !has(self.ref)) || (!has(self.capability) && has(self.kind) && self.kind == 'byo' && has(self.ref))",message="set either capability, or kind byo with ref"
// +kubebuilder:validation:XValidation:rule="(has(self.env) && !has(self.header) && !has(self.scheme)) || (!has(self.env) && has(self.header))",message="exactly one of env or header must be set, and scheme is only valid with header"
type MCPCredentialRef struct {
	// Capability is the goober capability whose scoped credential is resolved.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Capability string `json:"capability,omitempty" yaml:"capability,omitempty"`
	// Kind selects a non-capability credential source.
	// +kubebuilder:validation:Enum=byo
	// +optional
	Kind MCPCredentialKind `json:"kind,omitempty" yaml:"kind,omitempty"`
	// Ref names the operator-provided BYO credential.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +optional
	Ref string `json:"ref,omitempty" yaml:"ref,omitempty"`
	// Env names the environment variable exposed to a local stdio server.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Env string `json:"env,omitempty" yaml:"env,omitempty"`
	// Header names the HTTP header sent to a remote server.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Header string `json:"header,omitempty" yaml:"header,omitempty"`
	// Scheme optionally prefixes a remote header credential with "Bearer " or
	// "Basic ".
	// +kubebuilder:validation:Enum=bearer;basic
	// +optional
	Scheme MCPHeaderScheme `json:"scheme,omitempty" yaml:"scheme,omitempty"`
}

// MCPServer declares one external MCP server available to this goober. Exactly
// one of Command (local stdio) or URL (remote HTTP) must be configured.
// +kubebuilder:validation:XValidation:rule="(has(self.command) && !has(self.url)) || (!has(self.command) && has(self.url))",message="exactly one of command or url must be set"
// +kubebuilder:validation:XValidation:rule="has(self.command) || !has(self.args)",message="args are only valid with command"
type MCPServer struct {
	// Name is the invocation-local MCP server name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name" yaml:"name"`
	// Command starts a local stdio MCP server.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Command string `json:"command,omitempty" yaml:"command,omitempty"`
	// Args are passed directly to Command without shell interpretation.
	// +optional
	Args []string `json:"args,omitempty" yaml:"args,omitempty"`
	// URL is an HTTP(S) remote MCP endpoint.
	// +kubebuilder:validation:MinLength=1
	// +optional
	URL string `json:"url,omitempty" yaml:"url,omitempty"`
	// CredentialRefs bind first-party or named BYO credentials to this server.
	// +kubebuilder:validation:MaxItems=32
	// +optional
	CredentialRefs []MCPCredentialRef `json:"credentialRefs,omitempty" yaml:"credentialRefs,omitempty"`
}

// GooberSpec is the definition of a role-specialized AI worker. It declares
// everything needed to materialize the goober as ephemeral pods when a workflow
// invokes it (GBO-001, GBO-002).
type GooberSpec struct {
	// Gaggle is the name of the Gaggle this goober belongs to.
	// +kubebuilder:validation:Required
	Gaggle string `json:"gaggle" yaml:"gaggle"`
	// Role is the goober's role, e.g. "coder", "perf-hunter", "reviewer".
	// +kubebuilder:validation:Required
	Role string `json:"role" yaml:"role"`
	// DisplayName is the human-facing name shown on the portal dashboard.
	// +optional
	DisplayName string `json:"displayName,omitempty" yaml:"displayName,omitempty"`
	// Instructions points at the markdown file defining this goober's
	// behavior/persona/scope, relative to the goober definition directory. The
	// file is markdown with optional YAML frontmatter (see config-as-code docs).
	// +kubebuilder:validation:Required
	Instructions string `json:"instructions" yaml:"instructions"`
	// Harness is the agent harness this goober runs on.
	// +kubebuilder:validation:Enum=copilot;claude-code
	// +kubebuilder:default=copilot
	// +optional
	Harness Harness `json:"harness,omitempty" yaml:"harness,omitempty"`
	// Model selects the harness model. Values are scoped to Harness and
	// validated by its adapter before a run starts.
	// +optional
	Model string `json:"model,omitempty" yaml:"model,omitempty"`
	// HarnessOptions are harness-specific settings. The platform preserves the
	// map as opaque JSON values and the selected harness adapter validates it.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	HarnessOptions map[string]apiextensionsv1.JSON `json:"harnessOptions,omitempty" yaml:"harnessOptions,omitempty"`
	// TimeoutSeconds is the default per-attempt wall-clock bound for this
	// goober's agentic stages, overriding the harness's built-in 30m default
	// (internal/harness.DefaultTimeout). A stage's own Task.TimeoutSeconds, when
	// set, takes precedence over this — precedence is task > goober > built-in
	// default. Unset preserves the built-in default. Raise it for a goober that
	// does large work (a bigger implementation task legitimately exceeding 30m)
	// without annotating every stage that invokes it; leave it unset for
	// goobers whose work fits the default.
	// +kubebuilder:validation:Minimum=1
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty" yaml:"timeoutSeconds,omitempty"`
	// Capabilities are the capability grants this goober holds (e.g.
	// "github:issues:write", "repo:push", "telemetry:read"). A stage invoking
	// this goober may only use capabilities in this set; undeclared use fails
	// closed at compile time, and locally the credentials for an ungranted
	// capability are simply never materialized (ARCHITECTURE.md §5, SEC-042).
	// +optional
	Capabilities []string `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	// PolicyActions is the closed vocabulary of externally mutating actions
	// this goober's persona unconditionally prescribes. Every agentic task that
	// invokes the goober must redeclare these actions and their capabilities.
	// +optional
	PolicyActions []string `json:"policyActions,omitempty" yaml:"policyActions,omitempty"`
	// ConditionalPolicyActions lists persona actions that are prescribed only
	// when an invoking task explicitly declares the action and grants its
	// capability. This supports opt-in authority such as issue self-approval
	// without making it part of every invocation.
	// +optional
	ConditionalPolicyActions []string `json:"conditionalPolicyActions,omitempty" yaml:"conditionalPolicyActions,omitempty"`
	// Skills are the named skills available to this goober.
	// +optional
	Skills []string `json:"skills,omitempty" yaml:"skills,omitempty"`
	// Tools is the per-goober tool allowlist (default-deny). Only listed MCP
	// servers/tools are reachable from a run (GBO-Q2/SEC-Q4).
	// +optional
	Tools []string `json:"tools,omitempty" yaml:"tools,omitempty"`
	// MCPServers are external stdio or remote MCP servers materialized only for
	// this goober's harness invocation.
	// +listType=map
	// +listMapKey=name
	// +optional
	// +kubebuilder:validation:MaxItems=32
	MCPServers []MCPServer `json:"mcpServers,omitempty" yaml:"mcpServers,omitempty"`
	// ScaleFactor is the desired replica count for concurrent work. Increasing it
	// and redeploying yields more concurrent replicas, which claim work so no two
	// process the same item (GBO-030/GBO-031).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	// +optional
	ScaleFactor int32 `json:"scaleFactor,omitempty" yaml:"scaleFactor,omitempty"`
	// Workflows are the names of the workflow(s) that invoke this goober. A
	// goober may be referenced by multiple workflows (GBO-Q4); the per-task
	// invocation envelope differentiates behavior.
	// +optional
	Workflows []string `json:"workflows,omitempty" yaml:"workflows,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=gbo
// +kubebuilder:subresource:status

// Goober is a role-specialized AI worker defined as code.
type Goober struct {
	metav1.TypeMeta   `json:",inline" yaml:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty" yaml:"metadata,omitempty"`

	// +kubebuilder:validation:Required
	Spec GooberSpec `json:"spec" yaml:"spec"`
}

// +kubebuilder:object:root=true

// GooberList is a list of Goober objects.
type GooberList struct {
	metav1.TypeMeta `json:",inline" yaml:",inline"`
	metav1.ListMeta `json:"metadata,omitempty" yaml:"metadata,omitempty"`
	Items           []Goober `json:"items" yaml:"items"`
}
