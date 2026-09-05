package v1alpha1

import (
	"bytes"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TriggerType differentiates workflow archetypes without splitting the taxonomy:
// workflows run manually, consume backlog items, or react to a schedule,
// external signal, or GitHub webhook (WF-010, WF-013).
type TriggerType string

const (
	// TriggerManual declares a workflow that only runs through an explicit
	// `goobers run` invocation.
	TriggerManual TriggerType = "manual"
	// TriggerBacklogItem uses the provider's eligible-item count to fan out
	// scheduler runs.
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
// +kubebuilder:validation:XValidation:rule="!has(self.trustLabel) || self.type == 'backlog-item'",message="trustLabel is supported only for type=backlog-item"
// +kubebuilder:validation:XValidation:rule="!has(self.idleBackoff) || self.type == 'schedule'",message="idleBackoff is supported only for type=schedule"
type Trigger struct {
	// +kubebuilder:validation:Enum=manual;backlog-item;schedule;signal;webhook
	// +kubebuilder:validation:Required
	Type TriggerType `json:"type" yaml:"type"`
	// Selector configures backlog-item filtering. Keys are required labels and
	// values are ignored.
	// +optional
	Selector map[string]string `json:"selector,omitempty" yaml:"selector,omitempty"`
	// TrustLabel is the explicit SEC-047 approval label used to classify a
	// directly triggered backlog item as maintainer integrity. It is never
	// inferred from Selector, whose labels are routing criteria only.
	// +kubebuilder:validation:MinLength=1
	// +optional
	TrustLabel string `json:"trustLabel,omitempty" yaml:"trustLabel,omitempty"`
	// LabelPredicate is a CEL expression over the item's label set. The only
	// supported operations are string membership in `labels` and boolean
	// composition with &&, ||, and !. It is ANDed with Selector.
	// +kubebuilder:validation:MinLength=1
	// +optional
	LabelPredicate string `json:"labelPredicate,omitempty" yaml:"labelPredicate,omitempty"`
	// FieldPredicate is a CEL expression over the provider's typed native-field
	// projection. It is ANDed with Selector and fails when a referenced field is
	// unavailable.
	// +kubebuilder:validation:MinLength=1
	// +optional
	FieldPredicate string `json:"fieldPredicate,omitempty" yaml:"fieldPredicate,omitempty"`
	// Priority orders provider-backed polling when a quota window cannot cover
	// every due poll. Higher values are preserved first; equal values use the
	// scheduler's deterministic workflow ordering.
	// +optional
	Priority int32 `json:"priority,omitempty" yaml:"priority,omitempty"`
	// Schedule is a cron expression or interval (e.g. "@every 1h") for
	// type=schedule.
	// +optional
	Schedule string `json:"schedule,omitempty" yaml:"schedule,omitempty"`
	// IdleBackoff reduces no-work polling while preserving the configured
	// schedule whenever work is available. Omitted uses the default policy.
	// +optional
	IdleBackoff *IdleBackoff `json:"idleBackoff,omitempty" yaml:"idleBackoff,omitempty"`
	// Signal is the named external signal for type=signal.
	// +optional
	Signal string `json:"signal,omitempty" yaml:"signal,omitempty"`
	// Events are GitHub webhook event names (for example pull_request, issues,
	// or check_suite) for type=webhook.
	// +kubebuilder:validation:MinItems=1
	// +optional
	Events []string `json:"events,omitempty" yaml:"events,omitempty"`
	// Enabled toggles whether the scheduler honors this trigger without
	// removing it from the workflow. Omitting the field preserves the
	// historical behavior (the trigger is active). Setting it to false
	// disables the trigger for schedule, signal, webhook, and backlog-item
	// types; type=manual is unaffected because manual runs are always started
	// from the operator surface, not by the scheduler.
	// +optional
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
}

// IdleBackoff configures adaptive delay after consecutive scheduled runs find
// no work. Floor and Ceiling are Go duration strings.
type IdleBackoff struct {
	// Enabled defaults to true.
	// +optional
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	// Floor defaults to one minute.
	// +optional
	Floor string `json:"floor,omitempty" yaml:"floor,omitempty"`
	// Ceiling defaults to fifteen minutes.
	// +optional
	Ceiling string `json:"ceiling,omitempty" yaml:"ceiling,omitempty"`
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
	//
	// Zero means the same thing as unset here (falls back to 10) — NOT
	// "unlimited". That is the opposite convention from instance.yaml's
	// runConditions.maxParallelRuns, where zero means unlimited. There is
	// currently no way to express "no hourly budget" for a single workflow;
	// set an explicit large value instead if you want a high ceiling (#3360).
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
	// DesiredConcurrentRuns targets a minimum concurrent occupancy for queue-processing
	// workflows. When set and less than or equal to MaxConcurrentRuns, the scheduler maintains
	// refill intent to keep active runs at this level when eligible work is available.
	// Budget/deadline rejections retain refill intent and retry with backoff. A workflow
	// remains trigger-driven unless this is explicitly set (#3491).
	// +kubebuilder:validation:Minimum=1
	// +optional
	DesiredConcurrentRuns int32 `json:"desiredConcurrentRuns,omitempty" yaml:"desiredConcurrentRuns,omitempty"`
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
	// Experiment routes this task across two or three declared safety arms.
	// Variants overlay task inputs; selection and outcomes are journaled by the
	// runner and promotion remains an external approval decision.
	// +optional
	Experiment *BanditExperiment `json:"experiment,omitempty" yaml:"experiment,omitempty"`
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
	// MinimumIntegrity is the lowest provenance grade this task accepts from its
	// backlog item and context pointers. Empty preserves compatibility by
	// imposing no integrity admission policy.
	// +kubebuilder:validation:Enum=trusted;maintainer;unapproved;derived
	// +optional
	MinimumIntegrity Integrity `json:"minimumIntegrity,omitempty" yaml:"minimumIntegrity,omitempty"`
	// ContextFrom limits this task's context pointers to artifacts and verdicts
	// produced by the named tasks or gates. Empty preserves the historical
	// behavior of receiving every accumulated pointer.
	// System-generated pointers have no producing task or gate and are outside
	// the filter: an injected learning.episode[<seq>] correction survives it
	// unconditionally (see SelectContextPointers and ContextPointerClass).
	// Duplicate entries are rejected by api/validate (CTX001) rather than by a
	// CRD uniqueness marker, which Kubernetes refuses to install.
	// +optional
	ContextFrom []string `json:"contextFrom,omitempty" yaml:"contextFrom,omitempty"`
	// PolicyActions declares the closed vocabulary of externally mutating
	// actions this task may perform because a policy, persona, or verdict
	// prescribes them. The compiler maps each action to its required credential
	// capabilities and fails closed when the task does not declare every grant.
	// Agentic tasks must include every unconditional action declared by their
	// referenced goober's persona.
	// Known policy-bearing built-in commands must declare their complete action
	// set so policy changes cannot silently outrun capability admission.
	// +optional
	PolicyActions []string `json:"policyActions,omitempty" yaml:"policyActions,omitempty"`
	// NestedAgentPolicy controls child-agent delegation and context. It is
	// admitted before execution; omitted preserves legacy non-nested behavior.
	// +optional
	NestedAgentPolicy *NestedAgentPolicy `json:"nestedAgentPolicy,omitempty" yaml:"nestedAgentPolicy,omitempty"`
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
	// InputsFrom declares an explicit, small output->input handoff into this
	// task's Inputs: InputsFrom[inputKey] = outputKey. A bare outputKey reads the
	// immediately preceding task. DSL 2.0 also admits <stage>.<outputKey> and,
	// on a parallel join, <parallel>.<branch>.<stage>.<outputKey>; the stage may
	// be omitted only when that branch has one @join-terminal stage.
	// Unlike a gate (which receives
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
	// Workspace selects the filesystem workspace for this stage, for stages
	// that cannot express it through Run — i.e. AGENTIC tasks, which have no
	// DeterministicRun and were previously hardcoded to the writable repo
	// worktree. A deterministic task should keep using Run.Workspace, which
	// takes precedence when both are set.
	//
	// The motivating case is repo-readonly for an agentic research stage
	// inside a parallel branch (docs/design/static-fan-out-fan-in.md §6.5).
	// +kubebuilder:validation:Enum=repo;scratch;repo-readonly
	// +optional
	Workspace WorkspaceMode `json:"workspace,omitempty" yaml:"workspace,omitempty"`
	// Outbox declares workspace-relative paths (files or directories) this
	// stage durably exports into the run journal's outbox namespace
	// (runs/<id>/artifacts/outbox/**) on top of its ordinary result, for
	// output that isn't something that can PR against a repo — reports, wiki
	// content, JSON payloads, debugging artifacts (#1552). Opt-in and
	// additive: a stage that declares none is unaffected. A declared path
	// that does not exist when the stage completes is skipped, not an error;
	// a declared path that resolves outside the workspace (lexically or via
	// a symlink) fails the stage closed. Every attempt's export is bounded
	// by a fixed per-attempt file-count and aggregate byte limit enforced by
	// the journal, independent of how many paths are declared.
	// +kubebuilder:validation:MaxItems=32
	// +optional
	Outbox []string `json:"outbox,omitempty" yaml:"outbox,omitempty"`
	// OutboxMirrorPath overrides the workflow and gaggle local outbox mirror
	// root for this task. It is used only when Outbox exports at least one file.
	// The configured path must be absolute, or start with "~/".
	// +kubebuilder:validation:MinLength=1
	// +optional
	OutboxMirrorPath string `json:"outboxMirrorPath,omitempty" yaml:"outboxMirrorPath,omitempty"`
	// Next is the name of the next state (task or gate). Empty means terminal.
	// +optional
	Next string `json:"next,omitempty" yaml:"next,omitempty"`
	// RunsOn declares where this stage may execute (DSL 3.0, dsl-3.0.md §2):
	// OS, resource minimums, toolchain capability tags, and required runner
	// restrictions. Every field is optional — unspecified means no requirement
	// (explicit-complete semantics, D3). Interpreters before 3.0 refuse the
	// field; the compiler enforces that, not the shared schema.
	// +optional
	RunsOn *RunsOn `json:"runsOn,omitempty" yaml:"runsOn,omitempty"`
	// RepoFrom names the producer stage(s) whose run-branch state this repo
	// stage consumes — the declared repo-handoff edge (DSL 3.0, dsl-3.0.md §4).
	// Scalar or list in YAML; a list means "the run branch head as of the most
	// recent listed producer that executed". The 3.0 compiler computes the
	// required coverage as reaching definitions over the stage graph and
	// rejects an undeclared chain, an uncovered producer, or a dead entry
	// (WF022).
	// +optional
	RepoFrom RepoFrom `json:"repoFrom,omitempty" yaml:"repoFrom,omitempty"`
	// CommitsRepo declares that this deterministic stage's script/command
	// commits to the run branch — the explicit producer opt-in of DSL 3.0's
	// repo-handoff model (dsl-3.0.md §4, delivery decision 002). Agentic
	// non-readonly stages and the ref-advancing builtins are producers by
	// classification and never need it; a make/sh stage is a non-producer
	// unless it sets this. In 3.0 the runtime records the branch head around
	// every non-producer repo stage and fails closed on an undeclared advance.
	// +optional
	CommitsRepo bool `json:"commitsRepo,omitempty" yaml:"commitsRepo,omitempty"`
}

// BanditArm declares one variant and the gate strength required to evaluate it.
type BanditArm struct {
	Name    string            `json:"name" yaml:"name"`
	Variant map[string]string `json:"variant,omitempty" yaml:"variant,omitempty"`
	// GateLevel is the required safety strength for the gate evaluating this arm:
	// automated=1, agentic=2, human=3.
	GateLevel int `json:"gateLevel" yaml:"gateLevel"`
}

// BanditExperiment configures bounded exploration and promotion criteria.
type BanditExperiment struct {
	Seed              uint64      `json:"seed" yaml:"seed"`
	Arms              []BanditArm `json:"arms" yaml:"arms"`
	ExplorationBudget int         `json:"explorationBudget" yaml:"explorationBudget"`
	MinSamples        int         `json:"minSamples" yaml:"minSamples"`
	MaxFailureRate    float64     `json:"maxFailureRate" yaml:"maxFailureRate"`
	MinLift           float64     `json:"minLift" yaml:"minLift"`
	Confidence        float64     `json:"confidence" yaml:"confidence"`
	TrainWindow       int         `json:"trainWindow" yaml:"trainWindow"`
	EvalWindow        int         `json:"evalWindow" yaml:"evalWindow"`
	DefaultGateLevel  int         `json:"defaultGateLevel" yaml:"defaultGateLevel"`
}

// RunsOn is a stage's placement requirement block (DSL 3.0, dsl-3.0.md §2 /
// decision record D2). It is the scheduling surface; credential grants keep
// the separate `capabilities:` field unchanged.
type RunsOn struct {
	// OS is the required operating system — a validated enum, never a free
	// token (the #659 supersession). Empty means no OS requirement: placement
	// policy prefers, and will wait bounded for, a Linux-class runner when the
	// inventory has one.
	// +kubebuilder:validation:Enum=linux;windows;macOS
	// +optional
	OS string `json:"os,omitempty" yaml:"os,omitempty"`
	// CPU is the minimum CPU as a Kubernetes quantity string (e.g. "2000m").
	// Minimums become pod resource requests in mode 3; limits come from the
	// matched runner's ceiling, never from the stage. Advisory on local modes.
	// +optional
	CPU string `json:"cpu,omitempty" yaml:"cpu,omitempty"`
	// Memory is the minimum memory as a Kubernetes quantity string (e.g. "4Gi").
	// +optional
	Memory string `json:"memory,omitempty" yaml:"memory,omitempty"`
	// Disk is the minimum disk as a Kubernetes quantity string (e.g. "20Gi").
	// +optional
	Disk string `json:"disk,omitempty" yaml:"disk,omitempty"`
	// Capabilities is the open toolchain tag set — DSL 2.0's
	// requiredCapabilities moved, not re-invented (internal/runnercap grammar,
	// exact set membership, no ranges). os=* tokens are rejected here (CAP004):
	// the OS field above is the only platform vocabulary in a 3.0 document.
	// MaxItems bounds CRD CEL validation cost (#3168, dsl-3.0.md open point 7).
	// +kubebuilder:validation:MaxItems=32
	// +optional
	Capabilities []string `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	// Restrictions are isolation effects the matched runner must ENFORCE,
	// drawn from the closed v1 effect list (decision record D7): network:none,
	// network:allowlist, fs:readonly-except-workspace, tmp:ephemeral,
	// env:default-deny. Unknown tokens are rejected with a suggestion (CAP005).
	// MaxItems bounds CRD CEL validation cost (#3168, dsl-3.0.md open point 7).
	// +kubebuilder:validation:MaxItems=8
	// +optional
	Restrictions []string `json:"restrictions,omitempty" yaml:"restrictions,omitempty"`
}

// RepoFrom is a scalar-or-list stage reference list: YAML/JSON may spell one
// producer as a bare string or several as a list (dsl-3.0.md §4, delivery
// decision 001 — CI-repass lanes create true fan-in a scalar cannot express).
// It marshals back to the scalar form when it holds exactly one entry.
type RepoFrom []string

// UnmarshalJSON accepts either a single string or a list of strings.
// sigs.k8s.io/yaml routes YAML through JSON, so this covers both encodings.
func (r *RepoFrom) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var list []string
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return err
		}
		*r = list
		return nil
	}
	var scalar string
	if err := json.Unmarshal(trimmed, &scalar); err != nil {
		return fmt.Errorf("repoFrom must be a stage name or a list of stage names: %w", err)
	}
	*r = RepoFrom{scalar}
	return nil
}

// MarshalJSON emits the scalar spelling for a single producer and the list
// spelling otherwise, matching how authors write the field.
func (r RepoFrom) MarshalJSON() ([]byte, error) {
	if len(r) == 1 {
		return json.Marshal(r[0])
	}
	return json.Marshal([]string(r))
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

// RunControls tunes runner-level safety budgets. An omitted field inherits from
// the next broader scope: workflow, then gaggle, then instance defaults.
type RunControls struct {
	// MaxRepasses bounds how many times gates may route a run back to the same
	// already-completed stage before escalation, regardless of gate or outcome.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxRepasses int32 `json:"maxRepasses,omitempty" yaml:"maxRepasses,omitempty"`
	// StalledRunTimeout is the maximum period a running journal may remain
	// silent before the watchdog escalates it. It uses Go duration syntax.
	// +kubebuilder:validation:MinLength=1
	// +optional
	StalledRunTimeout string `json:"stalledRunTimeout,omitempty" yaml:"stalledRunTimeout,omitempty"`
	// MaxRunDuration is the maximum total wall-clock age of a run. Empty
	// disables the total-duration limit. It uses Go duration syntax.
	// +kubebuilder:validation:MinLength=1
	// +optional
	MaxRunDuration string `json:"maxRunDuration,omitempty" yaml:"maxRunDuration,omitempty"`
}

// DeterministicRun describes the code a deterministic task runs.
// +kubebuilder:validation:XValidation:rule="has(self.command) != has(self.script)",message="exactly one of command or script is required"
// +kubebuilder:validation:XValidation:rule="!has(self.syncBase) || !self.syncBase || !has(self.workspace) || self.workspace != 'scratch'",message="syncBase requires a repo workspace"
// The CEL rule only rejects an empty executable: a trim()-based whitespace test
// exceeds the apiserver's per-rule cost budget on an unbounded argv string, so
// whitespace-only names are rejected by the JSON Schema pattern and by semantic
// compilation instead (#3661).
// +kubebuilder:validation:XValidation:rule="!has(self.command) || size(self.command[0]) > 0",message="command[0] must name an executable"
type DeterministicRun struct {
	// Command is the command + args to execute.
	// +kubebuilder:validation:MinItems=1
	// +optional
	Command []string `json:"command,omitempty" yaml:"command,omitempty"`
	// Script is an inline script executed by the host's native command
	// interpreter: sh on Unix and cmd.exe on Windows. It is mutually exclusive
	// with Command.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Script string `json:"script,omitempty" yaml:"script,omitempty"`
	// Env is the explicit environment supplied to the command in addition to
	// the runner's minimal base environment and capability-scoped credentials.
	// +optional
	Env map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
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
	// WorkspaceRepo provisions a fresh worktree from the target repository on
	// the run branch, which the stage may commit to and push.
	WorkspaceRepo WorkspaceMode = "repo"
	// WorkspaceScratch provisions an empty disposable directory.
	WorkspaceScratch WorkspaceMode = "scratch"
	// WorkspaceRepoReadOnly provisions a worktree at the run's pinned base
	// revision in DETACHED HEAD — no branch name, so no branch-name collision
	// and nothing to merge afterwards.
	//
	// This exists because every ordinary repo workspace is created on ONE run
	// branch, and git refuses to check one branch out in two worktrees
	// simultaneously: two concurrently-executing repo-backed stages collide
	// outright. Read-only research fan-out (the quality-sprint shape) needs
	// repo content without needing to write it, so detaching removes the
	// collision entirely rather than inventing a per-branch naming and merge
	// policy (docs/design/static-fan-out-fan-in.md §6.5).
	WorkspaceRepoReadOnly WorkspaceMode = "repo-readonly"
)

// IsRepoBacked reports whether a workspace mode materialises the target
// repository, whether or not the stage may write to it.
func (m WorkspaceMode) IsRepoBacked() bool {
	return m == WorkspaceRepo || m == WorkspaceRepoReadOnly
}

// IsWritableRepo reports whether a workspace mode puts the stage on the run
// branch with the intent that it commit there.
func (m WorkspaceMode) IsWritableRepo() bool { return m == WorkspaceRepo }

// EffectiveWorkspace resolves the workspace mode a task DECLARES, applying
// the one precedence Task.Workspace documents: Run.Workspace when set
// (authoritative for a deterministic task), else Task.Workspace. Empty means
// the task declared nothing — and what "" provisions is the substrate's
// reading, not this function's: the local runner and the worker treat it as
// the historical writable repo worktree, a stage pod checks nothing out.
//
// This is the ONE resolution of that precedence. The runner, the engine's
// continuity record, its pod dispatch and the credential plane all decide
// something from the declared workspace (which worktree to cut, whether to
// hand the stage its predecessor's commits, which capability the checkout
// needs), and the first divergent private copy — reading Run.Workspace alone
// — silently dropped a predecessor's commits for a task-level `workspace:
// repo` (#3803 review). A shared method makes that divergence impossible to
// write rather than something a review has to catch.
func (t Task) EffectiveWorkspace() WorkspaceMode {
	return EffectiveWorkspace(t.Workspace, t.Run)
}

// EffectiveWorkspace is Task.EffectiveWorkspace over the two declarations
// carried separately, for a caller that holds them apart from the Task (the
// engine's pod dispatch input carries Run and the task-level Workspace as
// two fields).
func EffectiveWorkspace(task WorkspaceMode, run *DeterministicRun) WorkspaceMode {
	if run != nil && run.Workspace != "" {
		return run.Workspace
	}
	return task
}

// EffectiveWorkspace resolves the workspace an agentic gate's reviewer
// declares (AgenticGate.Workspace). Empty for an unset declaration and for a
// non-agentic gate, which evaluates in no workspace at all; as with
// Task.EffectiveWorkspace, what "" provisions is the substrate's reading.
func (g Gate) EffectiveWorkspace() WorkspaceMode {
	if g.Agentic == nil {
		return ""
	}
	return g.Agentic.Workspace
}

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
// +kubebuilder:validation:XValidation:rule="!has(self.maxRepasses) || self.evaluator != 'human'",message="maxRepasses is only valid for automated or agentic gates"
// +kubebuilder:validation:XValidation:rule="!has(self.runsOn) || self.evaluator == 'agentic'",message="runsOn is only valid for agentic gates"
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
	// MaxRepasses overrides the inherited target-stage re-entry budget when this
	// gate routes to an already-completed stage. It is valid only for automated
	// and agentic gates.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxRepasses int32 `json:"maxRepasses,omitempty" yaml:"maxRepasses,omitempty"`
	// RunsOn declares the placement an AGENTIC gate's reviewer requires (DSL
	// 3.0, dsl-3.0.md §2 "Gates"; Goobernetes-E2E-Core decision 001): the
	// identical placement block tasks carry, with the identical gaggle-floor
	// merge and the derived harness:<reviewer goober's harness> tag. Optional
	// — absent, the reviewer evaluates in the daemon/control plane exactly as
	// before the field existed. Valid only when evaluator=agentic (automated
	// and human gates are control-plane by definition, ruling 2), and a
	// declared block must carry cpu AND memory (ruling 5: the gaggle floor
	// has no quantities and a review is the most expensive stage class in a
	// lane, so an inherited envelope would silently under-provision).
	//
	// Honoured at execution by the engine (rulings 7–8): a gate pinned to a
	// non-self runner evaluates in a dispatcher-created pod on that runner's
	// queue — the reviewer runs in review mode, the pod computes the
	// reviewer diff itself and surrenders a Verdict the engine re-validates
	// — and a placement self satisfies pins self and evaluates in-process. A
	// DAEMON-scheduled run (internal/runner) has no gate dispatch arm yet:
	// there, a gate placement self cannot satisfy is refused at boot
	// (workflow.refused) rather than run outside its declared isolation.
	// Interpreters before 3.0 refuse the field; the compiler enforces that,
	// not the shared schema.
	// +optional
	RunsOn *RunsOn `json:"runsOn,omitempty" yaml:"runsOn,omitempty"`
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
	// Workspace selects the filesystem workspace the reviewer evaluates in.
	// Unset preserves the historical behaviour (a writable repo worktree).
	// +kubebuilder:validation:Enum=repo;scratch;repo-readonly
	// +optional
	Workspace WorkspaceMode `json:"workspace,omitempty" yaml:"workspace,omitempty"`
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
	// RunControls overrides this workflow's gaggle and instance run-control
	// budgets. Individual automated or agentic gates may further override
	// MaxRepasses.
	// +optional
	RunControls *RunControls `json:"runControls,omitempty" yaml:"runControls,omitempty"`
	// OutboxMirrorPath is the default local filesystem root where this
	// workflow mirrors durable journal outbox files. A task may override it.
	// The configured path must be absolute, or start with "~/".
	// +kubebuilder:validation:MinLength=1
	// +optional
	OutboxMirrorPath string `json:"outboxMirrorPath,omitempty" yaml:"outboxMirrorPath,omitempty"`
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
	// TutorScope declares this workflow as a Tutor-role definition and names
	// its topology tier (TUT-A4, Tutor v2 design doc §4.3): every tutor is
	// already confined to one gaggle via Gaggle above (the hard silo — there
	// is no cross-gaggle tutor), and TutorScope additionally states whether
	// this particular tutor's config-write is further confined to one target
	// workflow's own subtree (Tier=per-workflow, persona/gate/wiring) or
	// spans the whole gaggle's shared config (Tier=per-gaggle, shared
	// skills/validation/structure). Unset means this workflow is not a
	// tutor.
	// +optional
	TutorScope *TutorScope `json:"tutorScope,omitempty" yaml:"tutorScope,omitempty"`
	// Requires declares this workflow's provider-capability requirements
	// (CONF-6, #2079, docs/design/provider-contract-conformance.md §6) —
	// distinct from Task.RequiredCapabilities (runner/toolchain capabilities,
	// RRQ-1/#1101) and Task.Capabilities (credential grants,
	// internal/capability). Unset derives the requirement set from the
	// stages this workflow actually uses (e.g. a merge-pr stage implies
	// pr.merge); an explicit value here replaces that derivation entirely,
	// letting an author narrow or widen it. Checked at config-load against
	// the gaggle's connected provider's declared capabilities
	// (providers.Provider.Capabilities()) — an unmet requirement fails load
	// with a config-time message naming the workflow, the missing
	// capability, and the provider, never a mid-run stage error.
	// +optional
	Requires *WorkflowRequirements `json:"requires,omitempty" yaml:"requires,omitempty"`
	// Tasks are the work states of the machine.
	// +kubebuilder:validation:MaxItems=128
	// +optional
	Tasks []Task `json:"tasks,omitempty" yaml:"tasks,omitempty"`
	// Gates are the validation/branching states of the machine.
	// +optional
	Gates []Gate `json:"gates,omitempty" yaml:"gates,omitempty"`
	// Parallels are the fan-out/fan-in states of the machine: a parallel forks
	// the run into a statically-declared set of named branches and joins them at
	// a single successor once every branch has settled
	// (docs/design/static-fan-out-fan-in.md).
	// +optional
	Parallels []Parallel `json:"parallels,omitempty" yaml:"parallels,omitempty"`
}

// BranchFailurePolicy declares what a parallel does when one of its branches
// fails. It is required — there is no default — because parallelism without an
// explicit failure policy bakes ambiguity in permanently (#1310).
type BranchFailurePolicy string

const (
	// BranchFailFast abandons not-yet-started branches and cancels started
	// siblings at their next stage boundary, skips the join, and routes to
	// OnFailure.
	BranchFailFast BranchFailurePolicy = "fail_fast"
	// BranchAllOrNothing lets every branch finish, then skips the join and
	// routes to OnFailure if any branch failed.
	BranchAllOrNothing BranchFailurePolicy = "all_or_nothing"
	// BranchContinueOnError always runs the join, which owns the decision via
	// the branch completeness record.
	BranchContinueOnError BranchFailurePolicy = "continue_on_error"
)

// Branch is one statically-declared arm of a Parallel. Its body is ordinary
// tasks and gates declared in the same Tasks/Gates lists; a branch is not a
// nested scope but a named entry point into a subgraph the compiler proves is
// disjoint from every other branch's.
type Branch struct {
	// Name identifies the branch in journal events, the completeness record,
	// and branch-qualified inputsFrom references. Unique within its parallel.
	// +kubebuilder:validation:Required
	Name string `json:"name" yaml:"name"`
	// Start is the name of the branch's first state (task or gate).
	// +kubebuilder:validation:Required
	Start string `json:"start" yaml:"start"`
}

// Parallel is a fan-out/fan-in state. Branch width is fixed at author time;
// dynamic (data-driven) width is deliberately not supported (#817).
type Parallel struct {
	// Name identifies this state; it is a valid next/branches target like any
	// other state name.
	// +kubebuilder:validation:Required
	Name string `json:"name" yaml:"name"`
	// FailurePolicy declares what happens when a branch fails. Required.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=fail_fast;all_or_nothing;continue_on_error
	FailurePolicy BranchFailurePolicy `json:"failurePolicy" yaml:"failurePolicy"`
	// Branches are the statically-declared arms, at least two. Branch ids are
	// assigned by declaration order (from 1; 0 is the run's root branch), so
	// they are deterministic and reproducible across runs and runners.
	// +kubebuilder:validation:MinItems=2
	// +kubebuilder:validation:Required
	Branches []Branch `json:"branches" yaml:"branches"`
	// Join is the state that runs exactly once, after every branch has settled.
	// It may only be entered through this parallel.
	// +kubebuilder:validation:Required
	Join string `json:"join" yaml:"join"`
	// OnFailure is the transition target when the policy is fail_fast or
	// all_or_nothing and a branch failed — a state name or a reserved terminal
	// target, so failure is always a defined branch and never a silent stop
	// (mirroring GT-002). Required for those two policies and FORBIDDEN under
	// continue_on_error, where the join always runs and owns the decision;
	// declaring both would name two contradictory owners of the same failure.
	// +optional
	OnFailure string `json:"onFailure,omitempty" yaml:"onFailure,omitempty"`
	// BranchTimeoutSeconds bounds one branch. A branch exceeding it terminates
	// at its next stage boundary, is recorded timed-out, and is then handled as
	// a failure under the declared policy — so a branch that never settles is a
	// defined outcome rather than a hang.
	// +optional
	BranchTimeoutSeconds int32 `json:"branchTimeoutSeconds,omitempty" yaml:"branchTimeoutSeconds,omitempty"`
	// MaxConcurrentBranches bounds how many branches execute at once. Unset
	// means 1 — deterministic sequential execution unless the author opts into
	// concurrency. Concurrent repo-backed branches additionally require the
	// read-only workspace mode, because every stage worktree is otherwise
	// created on one run branch and git forbids two worktrees on one branch.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxConcurrentBranches int32 `json:"maxConcurrentBranches,omitempty" yaml:"maxConcurrentBranches,omitempty"`
}

// TutorScopeTier is the topology tier of a Tutor-role workflow (TUT-A4).
type TutorScopeTier string

const (
	// TutorScopePerWorkflow confines the tutor's config-write to one target
	// workflow's own subtree (persona/gate/wiring) — local, cheap, frequent,
	// low-blast.
	TutorScopePerWorkflow TutorScopeTier = "per-workflow"
	// TutorScopePerGaggle spans the whole gaggle's shared config (shared
	// skills, workflow-level tests, workflow structure, capability
	// declarations, shared gate calibration) — higher-blast, stronger
	// governance.
	TutorScopePerGaggle TutorScopeTier = "per-gaggle"
)

// TutorScope names a Tutor-role workflow's topology tier and, for a
// per-workflow tutor, the single workflow it is scoped to.
type TutorScope struct {
	// Tier is per-workflow or per-gaggle.
	// +kubebuilder:validation:Enum=per-workflow;per-gaggle
	// +kubebuilder:validation:Required
	Tier TutorScopeTier `json:"tier" yaml:"tier"`
	// Target names the workflow this per-workflow tutor is scoped to. It
	// must reference a workflow in the same gaggle. Required when Tier is
	// per-workflow; must be empty when Tier is per-gaggle.
	// +optional
	Target string `json:"target,omitempty" yaml:"target,omitempty"`
}

// WorkflowRequirements declares a workflow's non-runner requirements —
// today, the provider capabilities it needs (WorkflowSpec.Requires, CONF-6
// #2079).
type WorkflowRequirements struct {
	// Capabilities is the set of provider capability keys
	// (docs/design/provider-contract-conformance.md §3.1, e.g. "pr.merge",
	// "pr.landing.enqueue") this workflow requires. When set, it replaces
	// the derived-from-stages default entirely.
	// +optional
	Capabilities []string `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
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
