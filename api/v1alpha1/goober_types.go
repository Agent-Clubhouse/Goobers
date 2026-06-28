package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// Harness identifies the agent harness a goober runs on. v1 supports the GitHub
// Copilot agent harness only; a pluggable multi-harness abstraction is deferred
// (GBO-040, GBO-041).
type Harness string

const (
	// HarnessCopilot is the GitHub Copilot agent harness (v1 default).
	HarnessCopilot Harness = "copilot"
)

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
	// +kubebuilder:validation:Enum=copilot
	// +kubebuilder:default=copilot
	// +optional
	Harness Harness `json:"harness,omitempty" yaml:"harness,omitempty"`
	// Skills are the named skills available to this goober.
	// +optional
	Skills []string `json:"skills,omitempty" yaml:"skills,omitempty"`
	// Tools is the per-goober tool allowlist (default-deny). Only listed MCP
	// servers/tools are reachable from a run (GBO-Q2/SEC-Q4).
	// +optional
	Tools []string `json:"tools,omitempty" yaml:"tools,omitempty"`
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
