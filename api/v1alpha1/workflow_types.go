package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// TriggerType differentiates workflow archetypes without splitting the taxonomy:
// consumer workflows trigger on backlog items; producer workflows trigger on a
// schedule or external signal (WF-010, WF-013).
type TriggerType string

const (
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
	// +kubebuilder:validation:Enum=backlog-item;schedule;signal
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
	// +optional
	MaxRunsPerHour int32 `json:"maxRunsPerHour,omitempty" yaml:"maxRunsPerHour,omitempty"`
	// MaxChainDepth bounds how deep signal-triggered chains may go (WF-015).
	// +optional
	MaxChainDepth int32 `json:"maxChainDepth,omitempty" yaml:"maxChainDepth,omitempty"`
}

// TaskType is the execution kind of a task: code-driven or goober-executed.
type TaskType string

const (
	// TaskDeterministic runs code/scripts/integrations without a goober (TSK-020).
	TaskDeterministic TaskType = "deterministic"
	// TaskAgentic invokes a goober for agentic work (TSK-010..012).
	TaskAgentic TaskType = "agentic"
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
	// ExpectedOutputs are postconditions downstream gates can validate (TSK-003).
	// +optional
	ExpectedOutputs []string `json:"expectedOutputs,omitempty" yaml:"expectedOutputs,omitempty"`
	// Next is the name of the next state (task or gate). Empty means terminal.
	// +optional
	Next string `json:"next,omitempty" yaml:"next,omitempty"`
}

// DeterministicRun describes the code a deterministic task runs.
type DeterministicRun struct {
	// Command is the command + args to execute.
	// +kubebuilder:validation:Required
	Command []string `json:"command" yaml:"command"`
	// Image optionally overrides the container image the command runs in.
	// +optional
	Image string `json:"image,omitempty" yaml:"image,omitempty"`
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
	// Check names the coded check to run (e.g. "tests-pass", "coverage-gte").
	// +kubebuilder:validation:Required
	Check string `json:"check" yaml:"check"`
	// Params parameterize the check (e.g. {"threshold": "80"}).
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
	// Triggers declare when the scheduler may start a run (WF-010).
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:Required
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
