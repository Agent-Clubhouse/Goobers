package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// GaggleSpec defines a siloed workforce within an instance. A gaggle targets one
// project codebase and exactly one backlog (singleton), and contains its own
// goobers and workflows (which reference it by name). Isolation declares the
// target namespace + identity per gaggle (GAG-001..006, SEC-001/002). The
// active mode-3 worker routes every stage pod for this gaggle into its
// declared isolation.namespace, and verifies that namespace's existence and
// this worker's RBAC access to it before polling or dispatching any work
// (#4897). ServiceAccount selects the stage identity; IdentityRef federation is not yet consumed.
type GaggleSpec struct {
	// Cost overrides instance-wide external cost publication. An omitted or
	// null enabled value inherits the instance default; local accounting remains active.
	// +optional
	Cost *CostReporting `json:"cost,omitempty" yaml:"cost,omitempty"`
	// Enabled selects whether new runs may start for this gaggle. Null or
	// omitted means true (enabled). Setting false blocks new run starts
	// without touching schedule or backlog configuration; in-flight runs
	// finish normally.
	// +optional
	// +nullable
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
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
	// Health configures gaggle health observation, escalation, and the narrow
	// set of product-authorized idempotent repairs. Omitted uses conservative
	// defaults: observation is enabled and destructive behavior is disabled.
	// +optional
	Health *GaggleHealthPolicy `json:"health,omitempty" yaml:"health,omitempty"`
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
	// IssueOwnershipScope is the default ownership policy every workflow task
	// in this gaggle inherits for provider-visible issue writes. A task may
	// override the policy with ownershipAssignees / ownershipUnassigned inputs.
	// Empty leaves writes unrestricted, preserving legacy behavior.
	// +optional
	IssueOwnershipScope *IssueOwnershipScope `json:"issueOwnershipScope,omitempty" yaml:"issueOwnershipScope,omitempty"`
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

// GaggleHealthPolicy is the gaggle-scoped health contract. Runtime code resolves
// omitted values through the immutable built-in defaults before evaluation.
type GaggleHealthPolicy struct {
	// Enabled controls periodic evaluation. Null or omitted defaults to true.
	// +optional
	// +nullable
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	// EvaluationInterval is a Go duration. Omitted defaults to five minutes.
	// +optional
	EvaluationInterval string `json:"evaluationInterval,omitempty" yaml:"evaluationInterval,omitempty"`
	// Thresholds bound detector windows and retained evidence.
	// +optional
	Thresholds *GaggleHealthThresholds `json:"thresholds,omitempty" yaml:"thresholds,omitempty"`
	// Findings overrides handling for known finding codes. Unknown codes are
	// rejected; omission retains the code's hard safety policy.
	// +optional
	Findings map[string]GaggleFindingPolicy `json:"findings,omitempty" yaml:"findings,omitempty"`
	// Notifications configures bounded operator notifications and escalation.
	// +optional
	Notifications *GaggleHealthNotifications `json:"notifications,omitempty" yaml:"notifications,omitempty"`
	// EventWorkflow emits filtered health transitions to a workflow. It can
	// observe events but can never authorize or perform a repair.
	// +optional
	EventWorkflow *GaggleHealthEventWorkflow `json:"eventWorkflow,omitempty" yaml:"eventWorkflow,omitempty"`
}

// GaggleHealthThresholds controls health detector timing and history bounds.
type GaggleHealthThresholds struct {
	TriggerSilence       string `json:"triggerSilence,omitempty" yaml:"triggerSilence,omitempty"`
	NoProgress           string `json:"noProgress,omitempty" yaml:"noProgress,omitempty"`
	FlappingWindow       string `json:"flappingWindow,omitempty" yaml:"flappingWindow,omitempty"`
	FlappingCount        int32  `json:"flappingCount,omitempty" yaml:"flappingCount,omitempty"`
	ProlongedDegradation string `json:"prolongedDegradation,omitempty" yaml:"prolongedDegradation,omitempty"`
	EvidenceRetention    string `json:"evidenceRetention,omitempty" yaml:"evidenceRetention,omitempty"`
}

// GaggleFindingPolicy selects handling and an optional bounded severity
// override for one known finding code.
type GaggleFindingPolicy struct {
	// +kubebuilder:validation:Enum=observe;repair;escalate
	Mode string `json:"mode" yaml:"mode"`
	// +kubebuilder:validation:Enum=info;warning;error;critical
	Severity string `json:"severity,omitempty" yaml:"severity,omitempty"`
}

// GaggleHealthNotifications controls notification and escalation delivery.
type GaggleHealthNotifications struct {
	// Enabled defaults to true.
	// +optional
	// +nullable
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	// EscalateAfter is a Go duration after which an unresolved finding is
	// escalated. Omitted defaults to one hour.
	// +optional
	EscalateAfter string `json:"escalateAfter,omitempty" yaml:"escalateAfter,omitempty"`
	// MinimumSeverity suppresses lower-severity notifications.
	// +optional
	// +kubebuilder:validation:Enum=info;warning;error;critical
	MinimumSeverity string `json:"minimumSeverity,omitempty" yaml:"minimumSeverity,omitempty"`
}

// GaggleHealthEventWorkflow filters journaled health transitions delivered to
// a workflow. This is notification-only and has no repair authority.
type GaggleHealthEventWorkflow struct {
	Enabled      bool     `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	Workflow     string   `json:"workflow,omitempty" yaml:"workflow,omitempty"`
	EventTypes   []string `json:"eventTypes,omitempty" yaml:"eventTypes,omitempty"`
	FindingCodes []string `json:"findingCodes,omitempty" yaml:"findingCodes,omitempty"`
	// +kubebuilder:validation:Enum=info;warning;error;critical
	MinimumSeverity string `json:"minimumSeverity,omitempty" yaml:"minimumSeverity,omitempty"`
}

// IssueOwnershipScope constrains provider-visible issue writes to owned
// assignees while keeping unassigned handling independently configurable.
type IssueOwnershipScope struct {
	// Assignees are provider identities this gaggle may mutate. Empty means no
	// assigned-owner restriction, though Unassigned may still refuse unassigned
	// issues.
	// +optional
	Assignees []string `json:"assignees,omitempty" yaml:"assignees,omitempty"`
	// Unassigned controls whether unassigned issues are in scope. Empty behaves
	// as "allow" when Assignees is empty and "refuse" when Assignees is set.
	// +kubebuilder:validation:Enum=allow;refuse
	// +optional
	Unassigned string `json:"unassigned,omitempty" yaml:"unassigned,omitempty"`
}

// CostReporting controls provider-visible cost receipts and summaries, not
// collection of local usage measurements or operator cost queries.
type CostReporting struct {
	// Enabled selects external cost publication. Null or omitted inherits the
	// enclosing default; the built-in instance default is true.
	// +optional
	// +nullable
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
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

// GaggleIsolation declares the isolation boundary for a gaggle. Both the
// active mode-3 worker and the quarantined operator consume Namespace
// (#4897); IdentityRef is declared by neither yet.
type GaggleIsolation struct {
	// Namespace is the k8s namespace this gaggle's stage pods and secrets are
	// created in. The active mode-3 dispatcher routes every stage pod for
	// this gaggle here — resolved from the pod's own Attempt.Gaggle, never a
	// worker-wide default or fallback — and refuses to start dispatching
	// until it has verified the namespace exists and this worker's
	// credentials hold the RBAC grants dispatch needs there (#4897). Two
	// gaggles may declare the SAME namespace deliberately; that shared
	// topology is supported.
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace" yaml:"namespace"`
	// ServiceAccount selects the stage pod account. Empty defaults to goobers-stage;
	// default is an explicit opt-out. The account must disable token automount.
	// +optional
	ServiceAccount string `json:"serviceAccount,omitempty" yaml:"serviceAccount,omitempty"`
	// IdentityRef names the target per-gaggle Azure workload identity
	// (managed-identity federation). The active dispatcher does not consume
	// it yet; ServiceAccount selects the Kubernetes account independently.
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

// DefaultStageServiceAccount is the unprivileged account shipped by the reference.
const DefaultStageServiceAccount = "goobers-stage"

// EffectiveServiceAccount resolves the stage identity, including explicit default opt-out.
func (i GaggleIsolation) EffectiveServiceAccount() string {
	if i.ServiceAccount != "" {
		return i.ServiceAccount
	}
	return DefaultStageServiceAccount
}
