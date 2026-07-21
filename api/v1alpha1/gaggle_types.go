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
	// CICommand is the local CI-equivalent command (build + lint + tests) this
	// gaggle's deterministic `local-ci` stage runs in place of the command that
	// stage declares (the Go default `["make","ci"]`), so a foreign, non-Go
	// gaggle can gate its PRs on its own stack's suite (e.g.
	// `["npm","run","ci"]`, `["dotnet","test"]`) without rewriting the shared
	// workflow template (MGV-1/#1009, docs/design/v1/multi-gaggle-validation.md
	// §G2). Empty leaves the `local-ci` stage's declared command untouched, so a
	// single Go gaggle behaves exactly as before. A non-zero exit fails the gate
	// exactly as `make ci` does today, and a bad command only ever fails this
	// gaggle's own PRs — never another gaggle's.
	// +optional
	CICommand []string `json:"ciCommand,omitempty" yaml:"ciCommand,omitempty"`
	// RequiredCapabilities are the runner (toolchain/platform) capabilities every
	// run of this gaggle needs on the runner it executes on — e.g. `dotnet@8`,
	// `xcode`, `os=windows` (RRQ-1/#1101,
	// docs/design/v1/polyglot-stacks.md §5). These are NOT the credential grants
	// a Task declares (`internal/capability`, `repo:push` &c.): they are
	// free-form, version-parameterized claims a runner advertises statically
	// (instance.yaml `runner.capabilities`). The scheduler fails a run to
	// schedule — with a diagnostic naming the missing capability — when the
	// runner does not claim every entry here; a runner that falsely claims one it
	// lacks degrades to a runtime error, which the scheduler does not prevent.
	// Empty imposes no requirement, so an instance that declares none schedules
	// exactly as today.
	// +optional
	RequiredCapabilities []string `json:"requiredCapabilities,omitempty" yaml:"requiredCapabilities,omitempty"`
	// BranchNamespace is the refs/heads/ root this gaggle's run branches live
	// under — providers.BranchName produces "<branchNamespace><workflow>/<run>".
	// Empty defaults to providers.DefaultBranchNamespace ("goobers/"). It is the
	// single value three consumers derive from so they cannot drift (#965/#1010):
	// the run branch the worktree pushes, the mirror-fetch exclusion that
	// preserves that branch across a run's stages, and the PR-selector headPrefix
	// defaults. Retuning it lets one instance host gaggles that keep their run
	// branches in distinct namespaces; a value with no trailing "/" is treated as
	// if it had one. Most gaggles omit it and share the default.
	// +optional
	BranchNamespace string `json:"branchNamespace,omitempty" yaml:"branchNamespace,omitempty"`
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

// GagglePhase is a coarse lifecycle summary of a Gaggle.
type GagglePhase string

const (
	// GagglePhasePending means the gaggle has not yet been fully reconciled.
	GagglePhasePending GagglePhase = "Pending"
	// GagglePhaseReady means the namespace and all worker deployments are present.
	GagglePhaseReady GagglePhase = "Ready"
	// GagglePhaseDegraded means reconciliation ran but some workers are not ready.
	GagglePhaseDegraded GagglePhase = "Degraded"
)

// GaggleStatus reports the observed state of a Gaggle. The operator (M9) writes
// it via the status subresource.
type GaggleStatus struct {
	// ObservedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty" yaml:"observedGeneration,omitempty"`
	// Phase is a coarse lifecycle summary: Pending, Ready, or Degraded.
	// +optional
	Phase GagglePhase `json:"phase,omitempty" yaml:"phase,omitempty"`
	// GooberCount is the number of Goobers currently bound to this gaggle.
	// +optional
	GooberCount int32 `json:"gooberCount,omitempty" yaml:"gooberCount,omitempty"`
	// ReadyWorkers is the number of worker Deployments fully available.
	// +optional
	ReadyWorkers int32 `json:"readyWorkers,omitempty" yaml:"readyWorkers,omitempty"`
	// Conditions follow standard k8s conventions; "Ready" summarizes reconcile.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" yaml:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=gag
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Goobers",type=integer,JSONPath=`.status.gooberCount`

// Gaggle is a siloed workforce of goobers within an instance.
type Gaggle struct {
	metav1.TypeMeta   `json:",inline" yaml:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty" yaml:"metadata,omitempty"`

	// +kubebuilder:validation:Required
	Spec GaggleSpec `json:"spec" yaml:"spec"`
	// +optional
	Status GaggleStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GaggleList is a list of Gaggle objects.
type GaggleList struct {
	metav1.TypeMeta `json:",inline" yaml:",inline"`
	metav1.ListMeta `json:"metadata,omitempty" yaml:"metadata,omitempty"`
	Items           []Gaggle `json:"items" yaml:"items"`
}
