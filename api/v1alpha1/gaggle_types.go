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
	// SelfIdentity is this gaggle's provider login for assignment-aware backlog
	// operations. Empty inherits the instance-wide selfIdentity default.
	// +optional
	// +kubebuilder:validation:MinLength=1
	SelfIdentity string `json:"selfIdentity,omitempty" yaml:"selfIdentity,omitempty"`
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
	// RunControls overrides instance run-control defaults for every workflow in
	// this gaggle. A workflow may override either value again.
	// +optional
	RunControls *RunControls `json:"runControls,omitempty" yaml:"runControls,omitempty"`
	// OutboxMirrorPath is the default local filesystem root where workflows in
	// this gaggle mirror their durable journal outbox. A workflow or task may
	// override it. The local runner appends the run id and journal outbox layout
	// beneath this root; the journal remains the source of truth.
	// +kubebuilder:validation:MinLength=1
	// +optional
	OutboxMirrorPath string `json:"outboxMirrorPath,omitempty" yaml:"outboxMirrorPath,omitempty"`
	// Sandbox overrides the instance-wide isolation posture for this gaggle's
	// agentic stages (#1305). Effective posture is gaggle override, else the
	// instance.yaml sandbox block, else disabled — sandboxing is strictly
	// opt-in, so a gaggle that omits this behaves exactly as before.
	// +optional
	Sandbox *GaggleSandbox `json:"sandbox,omitempty" yaml:"sandbox,omitempty"`
	// Workcopies overrides the instance-level managed working-copy placement for
	// this gaggle. Root is an absolute base path; the gaggle name is appended.
	// +optional
	Workcopies *GaggleWorkcopies `json:"workcopies,omitempty" yaml:"workcopies,omitempty"`
	// RequireLabels is the default `requireLabels` value every workflow's
	// `backlog-query` task in this gaggle inherits, mirroring
	// BranchNamespace's gaggle-default/per-task-override shape (MIRC-2,
	// #1901, docs/design/v1/multi-instance-repo-coordination.md). A task that
	// declares its own `requireLabels` input fully replaces this default for
	// that task, exactly as a task's `headPrefix` replaces the gaggle's
	// BranchNamespace — never merged. Empty leaves every task's own
	// `requireLabels` (or its absence) untouched, so a gaggle that omits this
	// behaves exactly as before.
	// +optional
	RequireLabels []string `json:"requireLabels,omitempty" yaml:"requireLabels,omitempty"`
	// RunsOn is the gaggle-level placement floor (DSL 3.0, dsl-3.0.md §2): OS,
	// toolchain capability tags, and required runner restrictions that merge
	// into every stage of every workflow in this gaggle — capabilities and
	// restrictions union with the stage's own; an OS conflict between gaggle
	// and stage is a compile error, never a silent override. No quantities at
	// gaggle level. It is the 3.0 successor of RequiredCapabilities above and
	// activates only for gaggles whose workflows are pinned to DSL 3.0; the
	// 3.0 interpreter refuses a gaggle that still declares
	// RequiredCapabilities, and earlier interpreters refuse this field.
	// +optional
	RunsOn *GaggleRunsOn `json:"runsOn,omitempty" yaml:"runsOn,omitempty"`
	// Siblings declares other gaggles/instances this gaggle knows are
	// independently working the same target repo (MIRC-2, #1901). Each
	// sibling is identified by the repo it targets — never by gaggle/instance
	// name, which is purely local bookkeeping and carries zero cross-instance
	// meaning (docs/design/v1/multi-instance-repo-coordination.md, amended by
	// #1908). `goobers validate`/`goobers lint` warns (non-fatal) when a
	// declared sibling targets the same repo as this gaggle's own Project and
	// its declared RequireLabels are not disjoint from this gaggle's own
	// effective requireLabels (gaggle default, or a workflow's own override)
	// — the likely-dominant misconfiguration case for independently-
	// configured teams sharing one repo. A sibling targeting a different repo
	// never triggers a warning, regardless of label similarity. Declaring no
	// siblings is a no-op — purely additive, opt-in config.
	// +optional
	Siblings []GaggleSibling `json:"siblings,omitempty" yaml:"siblings,omitempty"`
}

// GaggleRunsOn is the gaggle-level placement floor of DSL 3.0 (dsl-3.0.md §2):
// the fields of a stage RunsOn that make sense for a whole gaggle — OS,
// capability tags, and restrictions, but never quantities. It merges into
// every stage as a floor: capabilities and restrictions union; an os conflict
// with a stage's own runsOn.os is a compile error.
type GaggleRunsOn struct {
	// OS every stage of this gaggle requires. Enum, same vocabulary as a
	// stage's runsOn.os.
	// +kubebuilder:validation:Enum=linux;windows;macOS
	// +optional
	OS string `json:"os,omitempty" yaml:"os,omitempty"`
	// Capabilities union into every stage's runsOn.capabilities. Same open
	// tag grammar (internal/runnercap); os=* tokens are rejected (CAP004).
	// +kubebuilder:validation:MaxItems=32
	// +optional
	Capabilities []string `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	// Restrictions union into every stage's runsOn.restrictions. Closed v1
	// effect list; unknown tokens are rejected with a suggestion (CAP005).
	// +kubebuilder:validation:MaxItems=8
	// +optional
	Restrictions []string `json:"restrictions,omitempty" yaml:"restrictions,omitempty"`
}

// GaggleWorkcopies configures managed working-copy placement for one gaggle.
type GaggleWorkcopies struct {
	// Root is an absolute base path for this gaggle's managed working copies.
	// +kubebuilder:validation:MinLength=1
	Root string `json:"root" yaml:"root"`
}

// GaggleSibling declares another gaggle/instance this gaggle knows is
// independently working the same target repo, for MIRC-2's sibling-overlap
// validation warning. This instance cannot read the sibling's live config, so
// RequireLabels is this gaggle's own trusted declaration of what the sibling
// currently uses — not something validated against the sibling itself.
type GaggleSibling struct {
	// Project is the repo the sibling gaggle targets — the sole match key
	// (provider/owner/name; Project is ADO-only, same as RepoRef). Gaggle
	// name is deliberately not part of this type: two instances naming a
	// gaggle the same string is coincidence with zero shared meaning.
	// +kubebuilder:validation:Required
	Project RepoRef `json:"project" yaml:"project"`
	// Label is a human-readable name for the sibling, used only in warning
	// messages (e.g. "Billing team") — never a match key.
	// +optional
	Label string `json:"label,omitempty" yaml:"label,omitempty"`
	// RequireLabels is this gaggle's own declaration of the sibling's
	// effective required-label scope, compared against this gaggle's own
	// effective requireLabels for overlap when Project matches.
	// +optional
	RequireLabels []string `json:"requireLabels,omitempty" yaml:"requireLabels,omitempty"`
}

// GaggleSandbox mirrors instance.yaml's sandbox block as a per-gaggle
// override: a posture declaration, never a mechanism selection.
type GaggleSandbox struct {
	// Agentic is the posture for agentic stages: "disabled" or "enforced".
	// Empty inherits the instance-wide posture.
	// +kubebuilder:validation:Enum=disabled;enforced
	// +optional
	Agentic string `json:"agentic,omitempty" yaml:"agentic,omitempty"`
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
