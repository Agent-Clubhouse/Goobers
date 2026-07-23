package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// TriggerType differentiates workflow archetypes without splitting the taxonomy:
// workflows run manually, consume backlog items, or react to a schedule,
// external signal, or GitHub webhook (WF-010, WF-013).
type TriggerType string

const (
	// TriggerManual declares a workflow that only runs through an explicit
	// `goobers run` invocation.
	TriggerManual TriggerType = "manual"
	// TriggerBacklogItem is reserved for V1 backlog routing. V0 accepts the
	// declaration but has no runtime consumer for it.
	TriggerBacklogItem TriggerType = "backlog-item"
	// TriggerSchedule fires on a schedule / time-since-last-run.
	TriggerSchedule TriggerType = "schedule"
	// TriggerSignal fires on an external signal (incl. another workflow's output,
	// always routed through the scheduler — WF-014).
	TriggerSignal TriggerType = "signal"
	// TriggerWebhook fires when the daemon receives a signed GitHub webhook for
	// one of the declared event names. Delivery reuses the signal scheduler path.
	TriggerWebhook TriggerType = "webhook"
)

// Trigger declares one condition under which the scheduler may start a run. A run
// starts only when a trigger fires AND readiness is satisfied (WF-011).
type Trigger struct {
	// +kubebuilder:validation:Enum=manual;backlog-item;schedule;signal;webhook
	// +kubebuilder:validation:Required
	Type TriggerType `json:"type" yaml:"type"`
	// Selector is reserved for V1 backlog-item routing via k8s-style label
	// matching (WF-040, SCH-010). V0 accepts but does not consume it.
	// +optional
	Selector map[string]string `json:"selector,omitempty" yaml:"selector,omitempty"`
	// Schedule is a cron expression or interval (e.g. "@every 1h") for
	// type=schedule.
	// +optional
	Schedule string `json:"schedule,omitempty" yaml:"schedule,omitempty"`
	// Signal is the named external signal for type=signal.
	// +optional
	Signal string `json:"signal,omitempty" yaml:"signal,omitempty"`
	// Events are GitHub webhook event names (for example pull_request, issues,
	// or check_suite) for type=webhook.
	// +kubebuilder:validation:MinItems=1
	// +optional
	Events []string `json:"events,omitempty" yaml:"events,omitempty"`
}

// ReadinessConditions bound when a run may start and how emergent chains are
// kept from running away (WF-011, WF-015).
type ReadinessConditions struct {
	// MaxConcurrentRuns caps simultaneous runs of this workflow.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	// +optional
	MaxConcurrentRuns int32 `json:"maxConcurrentRuns,omitempty" yaml:"maxConcurrentRuns,omitempty"`
	// MaxRunsPerHour is a run budget that bounds emergent chains (WF-015).
	// Unset falls back to a spec default of 10 (internal/localscheduler's
	// Conditions.Admit), not "unenforced" — every workflow gets some
	// guardrail against a runaway chain out of the box (#339).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=10
	// +optional
	MaxRunsPerHour int32 `json:"maxRunsPerHour,omitempty" yaml:"maxRunsPerHour,omitempty"`
	// MaxRunsPerDay is a native daily run budget (#340), enforced the same
	// way as MaxRunsPerHour over a rolling 24h window. Before this field
	// existed, a daily ceiling could only be faked by combining a specific
	// cron cadence with MaxRunsPerHour (e.g. 2x/day cadence x
	// maxRunsPerHour:1 = a ceiling of 2/day) — fragile and impossible to
	// reason about without mentally multiplying schedule-fires-per-day by
	// the hourly cap. Unset (0) means no daily cap — unlike MaxRunsPerHour,
	// this has no non-zero spec default, since MaxRunsPerHour's own default
	// already provides a baseline guardrail.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxRunsPerDay int32 `json:"maxRunsPerDay,omitempty" yaml:"maxRunsPerDay,omitempty"`
	// MaxChainDepth bounds how deep signal-triggered chains may go (WF-015).
	// +optional
	MaxChainDepth int32 `json:"maxChainDepth,omitempty" yaml:"maxChainDepth,omitempty"`
	// MaxOpenPRs caps how many un-merged PRs this workflow's runs may leave open
	// at once (#353). A looped implementation workflow branches every run off
	// origin/main; once several unmerged sibling PRs touch overlapping files they
	// become mutually un-mergeable as a set (the V0.3 ladder hit this). Gating
	// dispatch on the count of the workflow's own open PRs keeps the loop
	// merge-paced and the open set integrable, WITHOUT the runner doing cross-PR
	// rebase/conflict resolution — that is V0.5's merge-review/pr-remediation
	// layer (epic #357). Enforced as a readiness condition at admit time. 0 (the
	// default) disables the cap, so it is opt-in: only a PR-producing workflow
	// (implementation) sets it — capping curation/nomination, which open no PRs,
	// would wrongly block them on an unrelated open implementation PR.
	// +optional
	MaxOpenPRs int32 `json:"maxOpenPRs,omitempty" yaml:"maxOpenPRs,omitempty"`
}

// TaskType is the execution kind of a task: code-driven or goober-executed.
type TaskType string

const (
	// TaskDeterministic runs code/scripts/integrations without a goober (TSK-020).
	TaskDeterministic TaskType = "deterministic"
	// TaskAgentic invokes a goober for agentic work (TSK-010..012).
	TaskAgentic TaskType = "agentic"
)

const (
	// TaskOnTimeoutFail is the default Task.OnTimeout behavior: an agentic
	// session that hits its wall-clock timeout discards the attempt and lets
	// Task.Retry run, failing the run when the budget is exhausted.
	TaskOnTimeoutFail = "fail"
	// TaskOnTimeoutSalvage makes a timed-out agentic stage complete with
	// whatever it committed to the run branch, when that diff is non-empty, so
	// the workflow continues to its Next stage instead of discarding
	// actively-progressed work (#724).
	TaskOnTimeoutSalvage = "salvage"
)

// Task is a state in the workflow's state machine — the smallest unit of work the
// engine tracks. It is exactly one of deterministic or agentic (TSK-002).
type Task struct {
	// Name uniquely identifies this state within the workflow.
	// +kubebuilder:validation:Required
	Name string `json:"name" yaml:"name"`
	// +kubebuilder:validation:Enum=deterministic;agentic
	// +kubebuilder:validation:Required
	Type TaskType `json:"type" yaml:"type"`
	// Goal is the intended outcome of the task (TSK-001).
	// +kubebuilder:validation:Required
	Goal string `json:"goal" yaml:"goal"`
	// Goober names the goober invoked for an agentic task. Required when
	// type=agentic; must be empty when type=deterministic (TSK-010).
	// +optional
	Goober string `json:"goober,omitempty" yaml:"goober,omitempty"`
	// Run defines the code to execute for a deterministic task. Required when
	// type=deterministic; must be empty when type=agentic.
	// +optional
	Run *DeterministicRun `json:"run,omitempty" yaml:"run,omitempty"`
	// Inputs are static, task-specific inputs merged into the invocation
	// envelope's inputs blob at runtime.
	// +optional
	Inputs map[string]string `json:"inputs,omitempty" yaml:"inputs,omitempty"`
	// Capabilities are the capability grants this stage uses (e.g.
	// "github:issues:write", "repo:push"). For an agentic stage they MUST be a
	// subset of the invoked goober's granted capabilities; the compiler fails
	// closed on an undeclared capability (ARCHITECTURE.md §5, SEC-042).
	// +optional
	Capabilities []string `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	// PolicyActions declares the closed vocabulary of externally mutating
	// actions this task may perform because a policy, persona, or verdict
	// prescribes them. The compiler maps each action to its required credential
	// capabilities and fails closed when the task does not declare every grant.
	// Known policy-bearing built-in commands must declare their complete action
	// set so policy changes cannot silently outrun capability admission.
	// +optional
	PolicyActions []string `json:"policyActions,omitempty" yaml:"policyActions,omitempty"`
	// RequiredCapabilities are the runner (toolchain/platform) capabilities this
	// stage needs on the runner it executes on — e.g. `dotnet@8`, `xcode`,
	// `os=windows` (RRQ-1/#1101, docs/design/v1/polyglot-stacks.md §5). Distinct
	// from Capabilities above: those are credential grants validated against the
	// canonical internal/capability registry, whereas these are free-form,
	// version-parameterized runner claims matched at *schedule* time against a
	// runner's advertised capability set. A run is refused at schedule with a
	// diagnostic naming the missing capability when the runner does not claim
	// every entry (across every stage of the workflow plus its gaggle's own
	// RequiredCapabilities). Empty imposes no requirement.
	// +optional
	RequiredCapabilities []string `json:"requiredCapabilities,omitempty" yaml:"requiredCapabilities,omitempty"`
	// Retry declares how the runner retries this stage on failure. Retries are a
	// runner concern (WF-021): the policy is data, not behavior, so the compiled
	// machine stays deterministic. A retried attempt appears in the journal as a
	// new attempt, never overwritten history.
	// +optional
	Retry *RetryPolicy `json:"retry,omitempty" yaml:"retry,omitempty"`
	// TimeoutSeconds bounds one attempt's wall-clock execution. Unset preserves
	// the executor's default timeout.
	// +kubebuilder:validation:Minimum=1
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty" yaml:"timeoutSeconds,omitempty"`
	// Limits bound this task's duration and agent usage. TimeoutSeconds, when
	// set, is the authoritative duration and is copied into the invocation
	// envelope's Limits.MaxDurationSeconds.
	// +optional
	Limits *Limits `json:"limits,omitempty" yaml:"limits,omitempty"`
	// OnTimeout selects what the runner does when this stage's agentic session
	// hits its wall-clock timeout. Empty or "fail" (the default) discards the
	// timed-out attempt and lets Task.Retry run, failing the run once the budget
	// is exhausted —
	// historically this discarded real, in-progress work whose only remaining
	// step was CI (#724). "salvage" instead checks the run branch for a viable
	// committed diff and, if present, completes the stage with that diff so the
	// workflow continues to its Next stage rather than discarding the run. Only
	// meaningful for an agentic stage whose deliverable is its committed diff;
	// the compiler rejects it on a deterministic stage.
	// +kubebuilder:validation:Enum=fail;salvage
	// +optional
	OnTimeout string `json:"onTimeout,omitempty" yaml:"onTimeout,omitempty"`
	// ExpectedOutputs declares intended task postconditions. The V0 local runner
	// accepts but does not enforce this field; validation emits VER003 when set.
	// +optional
	ExpectedOutputs []string `json:"expectedOutputs,omitempty" yaml:"expectedOutputs,omitempty"`
	// ContinueOnError makes a ResultFailure best-effort: the failed status is
	// journaled and remains visible to a following gate, but the runner advances
	// to Next instead of failing the run. Outputs from the failed task are
	// discarded so downstream tasks cannot consume partial results.
	// +optional
	ContinueOnError bool `json:"continueOnError,omitempty" yaml:"continueOnError,omitempty"`
	// InputsFrom declares an explicit, small output->input handoff from the
	// immediately preceding task's ResultEnvelope.Outputs into this task's
	// Inputs: InputsFrom[inputKey] = outputKey. Unlike a gate (which receives
	// every upstream Output key flattened automatically, per
	// internal/gate/automated.go's runner-contract convention — a gate never
	// mutates run state, so a wide-open read is safe), a task-to-task handoff
	// can feed a stage's actual behavior, so it requires an explicit
	// declaration per key rather than blanket propagation — an auditable,
	// named data-flow edge in the DSL instead of an implicit wide-open
	// channel between arbitrary tasks. A declared outputKey missing from the
	// preceding task's Outputs fails the stage closed (the declaration is a
	// contract, not a hint).
	// +optional
	InputsFrom map[string]string `json:"inputsFrom,omitempty" yaml:"inputsFrom,omitempty"`
	// Next is the name of the next state (task or gate). Empty means terminal.
	// +optional
	Next string `json:"next,omitempty" yaml:"next,omitempty"`
}

// RetryPolicy declares how many times, and how far apart, the runner retries a
// failed stage. Backoff is a constant (not exponential-with-jitter) so the
// declared policy is fully deterministic; wall-clock waits happen in the runner,
// never in the compiled machine.
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts including the first (>=1). 1
	// means no retry.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Required
	MaxAttempts int32 `json:"maxAttempts" yaml:"maxAttempts"`
	// BackoffSeconds is the constant delay between attempts, in seconds.
	// +kubebuilder:validation:Minimum=0
	// +optional
	BackoffSeconds int32 `json:"backoffSeconds,omitempty" yaml:"backoffSeconds,omitempty"`
}

// DeterministicRun describes the code a deterministic task runs.
// +kubebuilder:validation:XValidation:rule="!has(self.syncBase) || !self.syncBase || !has(self.workspace) || self.workspace != 'scratch'",message="syncBase requires a repo workspace"
type DeterministicRun struct {
	// Command is the command + args to execute.
	// +kubebuilder:validation:Required
	Command []string `json:"command" yaml:"command"`
	// Env is the explicit environment supplied to the command in addition to
	// the runner's minimal base environment and capability-scoped credentials.
	// +optional
	Env map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	// Image optionally selects the command's container image. The V0 local
	// runner executes commands directly and does not honor this field;
	// validation emits VER003 when set.
	// +optional
	Image string `json:"image,omitempty" yaml:"image,omitempty"`
	// Network selects the command's network access. Empty inherits the host
	// network. "none" denies network access to the command and its descendants.
	// +kubebuilder:validation:Enum=none
	// +optional
	Network NetworkMode `json:"network,omitempty" yaml:"network,omitempty"`
	// Workspace selects where the command runs. Empty or "repo" provisions the
	// workflow's repository worktree. "scratch" provisions an empty, disposable
	// directory and requires no repository connection.
	// +kubebuilder:validation:Enum=repo;scratch
	// +optional
	Workspace WorkspaceMode `json:"workspace,omitempty" yaml:"workspace,omitempty"`
	// SyncBase merges the freshly fetched base ref into an existing run branch
	// before the command executes. It is valid only with a repository workspace.
	// +optional
	SyncBase bool `json:"syncBase,omitempty" yaml:"syncBase,omitempty"`
}

// NetworkMode selects the network access available to a deterministic command.
type NetworkMode string

const (
	// NetworkNone denies network access to the command and its descendants.
	NetworkNone NetworkMode = "none"
)

// WorkspaceMode selects the filesystem workspace for a deterministic command.
type WorkspaceMode string

const (
	// WorkspaceRepo provisions a fresh worktree from the target repository.
	WorkspaceRepo WorkspaceMode = "repo"
	// WorkspaceScratch provisions an empty disposable directory.
	WorkspaceScratch WorkspaceMode = "scratch"
)

// EvaluatorKind is the pluggable evaluator a gate uses. A gate has exactly one
// (GT-003, GT-016).
type EvaluatorKind string

const (
	// EvaluatorAutomated runs a coded check over task outputs (GT-010).
	EvaluatorAutomated EvaluatorKind = "automated"
	// EvaluatorAgentic invokes a scoped reviewer goober for a verdict (GT-011).
	EvaluatorAgentic EvaluatorKind = "agentic"
	// EvaluatorHuman pauses for an explicit human decision (GT-012).
	EvaluatorHuman EvaluatorKind = "human"
)

// Gate is a validation state that evaluates a condition and branches the flow. A
// failing/negative outcome MUST follow a defined branch — never a silent pass
// (GT-002).
type Gate struct {
	// Name uniquely identifies this state within the workflow.
	// +kubebuilder:validation:Required
	Name string `json:"name" yaml:"name"`
	// Evaluator selects the pluggable evaluator kind.
	// +kubebuilder:validation:Enum=automated;agentic;human
	// +kubebuilder:validation:Required
	Evaluator EvaluatorKind `json:"evaluator" yaml:"evaluator"`
	// Automated configures an automated evaluator. Set iff evaluator=automated.
	// +optional
	Automated *AutomatedGate `json:"automated,omitempty" yaml:"automated,omitempty"`
	// Agentic configures an agentic reviewer evaluator. Set iff evaluator=agentic.
	// +optional
	Agentic *AgenticGate `json:"agentic,omitempty" yaml:"agentic,omitempty"`
	// Human configures a human evaluator. Set iff evaluator=human.
	// +optional
	Human *HumanGate `json:"human,omitempty" yaml:"human,omitempty"`
	// Branches maps an outcome to the next state name. Supports more than two
	// branches (GT-004). The "pass" key is the success branch. The optional
	// "escalate" control branch routes runner-forced escalation through a
	// workflow state; when absent, escalation terminates at @escalate.
	// +kubebuilder:validation:Required
	Branches map[string]string `json:"branches" yaml:"branches"`
}

// AutomatedGate runs a deterministic coded check.
type AutomatedGate struct {
	// Check names a built-in coded check such as "status-equals",
	// "output-numeric-lte", or "output-matches".
	// +kubebuilder:validation:Required
	Check string `json:"check" yaml:"check"`
	// Params parameterize the check (e.g. {"key": "coverage", "threshold": "80"}).
	// +optional
	Params map[string]string `json:"params,omitempty" yaml:"params,omitempty"`
	// TimeoutSeconds bounds one evaluator attempt.
	// +kubebuilder:validation:Minimum=1
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty" yaml:"timeoutSeconds,omitempty"`
	// Retry declares the evaluator retry bound. Runtime retry semantics are
	// implemented separately from this declarative contract.
	// +optional
	Retry *RetryPolicy `json:"retry,omitempty" yaml:"retry,omitempty"`
	// PollIntervalSeconds declares remote CI polling cadence for checks that
	// poll, such as ci-status (GT-020).
	// +kubebuilder:validation:Minimum=1
	// +optional
	PollIntervalSeconds int32 `json:"pollIntervalSeconds,omitempty" yaml:"pollIntervalSeconds,omitempty"`
}

// AgenticGate invokes a scoped reviewer goober.
type AgenticGate struct {
	// Goober names the reviewer goober that returns a Verdict.
	// +kubebuilder:validation:Required
	Goober string `json:"goober" yaml:"goober"`
	// TimeoutSeconds bounds one reviewer attempt.
	// +kubebuilder:validation:Minimum=1
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty" yaml:"timeoutSeconds,omitempty"`
	// Retry declares the evaluator retry bound. Runtime retry semantics are
	// implemented separately from this declarative contract.
	// +optional
	Retry *RetryPolicy `json:"retry,omitempty" yaml:"retry,omitempty"`
}

// HumanGate pauses for an explicit human decision, surfaced in the portal.
type HumanGate struct {
	// Approvers optionally restricts who may approve (Entra principals/groups).
	// +optional
	Approvers []string `json:"approvers,omitempty" yaml:"approvers,omitempty"`
	// TimeoutSeconds optionally bounds how long the gate waits before escalating.
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty" yaml:"timeoutSeconds,omitempty"`
	// OnTimeout is the behavior when the timeout elapses (GT-013).
	// +kubebuilder:validation:Enum=remind;escalate;reject
	// +optional
	OnTimeout string `json:"onTimeout,omitempty" yaml:"onTimeout,omitempty"`
}

// WorkflowSpec defines a process as a deterministic state machine of Tasks and
// Gates, started by the scheduler on trigger + readiness (WF-001..016).
type WorkflowSpec struct {
	// Gaggle is the name of the Gaggle this workflow belongs to (WF-003).
	// +kubebuilder:validation:Required
	Gaggle string `json:"gaggle" yaml:"gaggle"`
	// DisplayName is the human-facing name shown on the portal.
	// +optional
	DisplayName string `json:"displayName,omitempty" yaml:"displayName,omitempty"`
	// Triggers declare when the scheduler may start a run (WF-010). A single
	// type=manual trigger declares a workflow that never auto-fires.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="!self.exists(t, t.type == 'manual') || size(self) == 1",message="type=manual must be the only trigger"
	Triggers []Trigger `json:"triggers" yaml:"triggers"`
	// Readiness bounds when a run may start (WF-011).
	// +optional
	Readiness ReadinessConditions `json:"readiness,omitempty" yaml:"readiness,omitempty"`
	// Start is the name of the first state (task or gate) of the machine.
	// +kubebuilder:validation:Required
	Start string `json:"start" yaml:"start"`
	// DocsRoots declares the in-repo documentation roots this workflow is
	// responsible for keeping current (docs-updater, epic #472/#1016). It is an
	// ordered list of repo-relative paths — files or directories, e.g. "docs",
	// "docs/design", "README.md", "ARCHITECTURE.md". When set it does two things:
	// the docs-drift signal-gather stage (docs-churn, #1015) groups the churn it
	// reports by whether a change landed under a declared root, and the write
	// boundary confines the run's PR to these roots (mirrors the
	// confineToConfigRoot/configRoot boundary open-pr already honors), so a docs
	// run can never touch code. Same-repo, in-repo roots only in Phase 1;
	// separate-repo / wiki sinks are their own gated children (#1019/#1020/#1021).
	// A declared root must be non-empty, repo-relative, and must not escape the
	// repository (validated at config-load); `goobers validate` additionally
	// rejects a root that does not exist in the repository.
	// +optional
	DocsRoots []string `json:"docsRoots,omitempty" yaml:"docsRoots,omitempty"`
	// Tasks are the work states of the machine.
	// +optional
	Tasks []Task `json:"tasks,omitempty" yaml:"tasks,omitempty"`
	// Gates are the validation/branching states of the machine.
	// +optional
	Gates []Gate `json:"gates,omitempty" yaml:"gates,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=wf
// +kubebuilder:subresource:status

// Workflow is a defined process modeled as a deterministic state machine.
type Workflow struct {
	metav1.TypeMeta   `json:",inline" yaml:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty" yaml:"metadata,omitempty"`

	// DSLVersion is the language version this workflow was authored against.
	// +kubebuilder:validation:Pattern="^[0-9]+\\.[0-9]+$"
	// +optional
	DSLVersion string `json:"dslVersion,omitempty" yaml:"dslVersion,omitempty"`

	// +kubebuilder:validation:Required
	Spec WorkflowSpec `json:"spec" yaml:"spec"`
}

// +kubebuilder:object:root=true

// WorkflowList is a list of Workflow objects.
type WorkflowList struct {
	metav1.TypeMeta `json:",inline" yaml:",inline"`
	metav1.ListMeta `json:"metadata,omitempty" yaml:"metadata,omitempty"`
	Items           []Workflow `json:"items" yaml:"items"`
}
