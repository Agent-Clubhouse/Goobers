package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// TriggerType differentiates workflow archetypes without splitting the taxonomy:
// workflows run manually, consume backlog items, or react to a schedule or
// external signal (WF-010, WF-013).
type TriggerType string

const (
	// TriggerManual declares a workflow that only runs through an explicit
	// `goobers run` invocation.
	TriggerManual TriggerType = "manual"
	// TriggerBacklogItem fires when a matching backlog item becomes available.
	TriggerBacklogItem TriggerType = "backlog-item"
	// TriggerSchedule fires on a schedule / time-since-last-run.
	TriggerSchedule TriggerType = "schedule"
	// TriggerSignal fires on an external signal (incl. another workflow's output,
	// always routed through the scheduler — WF-014).
	TriggerSignal TriggerType = "signal"
)

// Trigger declares one condition under which the scheduler may start a run. A run
// starts only when a trigger fires AND readiness is satisfied (WF-011).
type Trigger struct {
	// +kubebuilder:validation:Enum=manual;backlog-item;schedule;signal
	// +kubebuilder:validation:Required
	Type TriggerType `json:"type" yaml:"type"`
	// Selector routes backlog items to this workflow via k8s-style label matching
	// (WF-040, SCH-010). Used with type=backlog-item.
	// +optional
	Selector map[string]string `json:"selector,omitempty" yaml:"selector,omitempty"`
	// Schedule is a cron expression or interval (e.g. "@every 1h") for
	// type=schedule.
	// +optional
	Schedule string `json:"schedule,omitempty" yaml:"schedule,omitempty"`
	// Signal is the named external signal for type=signal.
	// +optional
	Signal string `json:"signal,omitempty" yaml:"signal,omitempty"`
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
	// Retry declares how the runner retries this stage on failure. Retries are a
	// runner concern (WF-021): the policy is data, not behavior, so the compiled
	// machine stays deterministic. A retried attempt appears in the journal as a
	// new attempt, never overwritten history.
	// +optional
	Retry *RetryPolicy `json:"retry,omitempty" yaml:"retry,omitempty"`
	// OnTimeout selects what the runner does when this stage's agentic session
	// hits its wall-clock timeout (the harness session timeout — currently a
	// flat 30m, internal/harness.DefaultTimeout; not yet per-stage configurable,
	// #151). Empty or "fail" (the default) discards the timed-out attempt and
	// lets Task.Retry run, failing the run once the budget is exhausted —
	// historically this discarded real, in-progress work whose only remaining
	// step was CI (#724). "salvage" instead checks the run branch for a viable
	// committed diff and, if present, completes the stage with that diff so the
	// workflow continues to its Next stage rather than discarding the run. Only
	// meaningful for an agentic stage whose deliverable is its committed diff;
	// the compiler rejects it on a deterministic stage.
	// +kubebuilder:validation:Enum=fail;salvage
	// +optional
	OnTimeout string `json:"onTimeout,omitempty" yaml:"onTimeout,omitempty"`
	// ExpectedOutputs are postconditions downstream gates can validate (TSK-003).
	// +optional
	ExpectedOutputs []string `json:"expectedOutputs,omitempty" yaml:"expectedOutputs,omitempty"`
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
type DeterministicRun struct {
	// Command is the command + args to execute.
	// +kubebuilder:validation:Required
	Command []string `json:"command" yaml:"command"`
	// Image optionally overrides the container image the command runs in.
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
	// branches (GT-004). The "pass" key is the success branch.
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
}

// AgenticGate invokes a scoped reviewer goober.
type AgenticGate struct {
	// Goober names the reviewer goober that returns a Verdict.
	// +kubebuilder:validation:Required
	Goober string `json:"goober" yaml:"goober"`
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
