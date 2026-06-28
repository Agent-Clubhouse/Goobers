package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// GaggleSpec defines a siloed workforce within an instance. A gaggle targets one
// project codebase and exactly one backlog (singleton), and contains its own
// goobers and workflows (which reference it by name). Isolation is realized as a
// namespace + identity per gaggle (GAG-001..006, SEC-001/002).
type GaggleSpec struct {
	// DisplayName is the human-facing name shown on the portal dashboard.
	// +optional
	DisplayName string `json:"displayName,omitempty" yaml:"displayName,omitempty"`
	// Project is the codebase this gaggle works on.
	// +kubebuilder:validation:Required
	Project RepoRef `json:"project" yaml:"project"`
	// Backlog is the singleton source of work-item truth for this gaggle.
	// +kubebuilder:validation:Required
	Backlog BacklogRef `json:"backlog" yaml:"backlog"`
	// Isolation declares the per-gaggle boundary (namespace + workload identity).
	// +kubebuilder:validation:Required
	Isolation GaggleIsolation `json:"isolation" yaml:"isolation"`
	// AdditionalRepos are optional extra repos a less-standard gaggle may target;
	// the backlog and infra/config repos always remain singletons (GAG-007).
	// +optional
	AdditionalRepos []RepoRef `json:"additionalRepos,omitempty" yaml:"additionalRepos,omitempty"`
}

// GaggleIsolation captures the isolation boundary for a gaggle: its Kubernetes
// namespace and the workload identity its runs assume.
type GaggleIsolation struct {
	// Namespace is the k8s namespace this gaggle's pods/secrets live in. Must be
	// unique per gaggle so credentials/work/telemetry do not leak across gaggles.
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace" yaml:"namespace"`
	// IdentityRef names the per-gaggle Azure workload identity (managed-identity
	// federation) used to reach Key Vault, providers, and telemetry.
	// +optional
	IdentityRef string `json:"identityRef,omitempty" yaml:"identityRef,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=gag
// +kubebuilder:subresource:status

// Gaggle is a siloed workforce of goobers within an instance.
type Gaggle struct {
	metav1.TypeMeta   `json:",inline" yaml:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty" yaml:"metadata,omitempty"`

	// +kubebuilder:validation:Required
	Spec GaggleSpec `json:"spec" yaml:"spec"`
}

// +kubebuilder:object:root=true

// GaggleList is a list of Gaggle objects.
type GaggleList struct {
	metav1.TypeMeta `json:",inline" yaml:",inline"`
	metav1.ListMeta `json:"metadata,omitempty" yaml:"metadata,omitempty"`
	Items           []Gaggle `json:"items" yaml:"items"`
}
