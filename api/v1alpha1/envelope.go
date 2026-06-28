package v1alpha1

// This file defines the three canonical wire envelopes every component exchanges
// at runtime. They are plain Go types (JSON-tagged) — NOT CRDs — so the
// scheduler, providers, gate evaluators, and goober runtime can import them
// without pulling in CRD machinery. JSON Schemas in api/schemas/ mirror these
// shapes and are the cross-language contract (TSK-Q1/TSK-Q2, GT-Q1).

// ---------------------------------------------------------------------------
// Invocation envelope — handed to a task (agentic via the goober invocation
// hook, or to a deterministic task runner) when the workflow advances.
// ---------------------------------------------------------------------------

// InvocationEnvelope is the standard context/data block delivered to a task at
// invocation (GBO-012, TSK-011). Shape: taskId, workflowId, runId, gaggle, item,
// goal, repoRef, upstreamOutputs, limits, inputs.
//
// This is a runtime wire envelope, not a Kubernetes object, so it is excluded
// from controller-gen DeepCopy generation (its free-form Inputs map cannot be
// deep-copied generically).
// +kubebuilder:object:generate=false
type InvocationEnvelope struct {
	// TaskID identifies this task instance within the run.
	TaskID string `json:"taskId"`
	// WorkflowID identifies the workflow definition being executed.
	WorkflowID string `json:"workflowId"`
	// RunID identifies this run (the OpenTelemetry trace id for the run).
	RunID string `json:"runId"`
	// Gaggle is the gaggle this run belongs to.
	Gaggle string `json:"gaggle"`
	// Item is the backlog item / trigger payload that started the run. Nil for
	// schedule/signal-triggered producer runs with no originating item.
	Item *BacklogItem `json:"item,omitempty"`
	// Goal is the intended outcome of this task (from the task definition).
	Goal string `json:"goal"`
	// RepoRef is the target repository for this run (fresh checkout per run).
	RepoRef RepoRef `json:"repoRef"`
	// UpstreamOutputs are the results of prior tasks in this run, keyed by task
	// name, so a task can build on earlier work.
	UpstreamOutputs map[string]ResultEnvelope `json:"upstreamOutputs,omitempty"`
	// Limits bound this task's execution (duration/tokens/cost).
	Limits Limits `json:"limits"`
	// Inputs are task-specific inputs (the task definition's static inputs plus
	// any dynamically supplied values).
	Inputs map[string]interface{} `json:"inputs,omitempty"`
}

// BacklogItem is a provider-neutral mirror of a unit of work. The backlog
// remains the source of truth; this is the snapshot handed to a run.
type BacklogItem struct {
	// ID is the provider-native item id (issue number, work-item id).
	ID string `json:"id"`
	// Provider is the backing system the item came from.
	Provider Provider `json:"provider"`
	// Title is the item's short title.
	Title string `json:"title,omitempty"`
	// Body is the item's description/details.
	Body string `json:"body,omitempty"`
	// URL links back to the item in the provider.
	URL string `json:"url,omitempty"`
	// Labels are the item's labels (used by workflow selectors for routing).
	Labels []string `json:"labels,omitempty"`
}

// Limits bound a task's execution. Zero values mean "no explicit limit".
type Limits struct {
	// MaxDurationSeconds caps wall-clock time for the task.
	MaxDurationSeconds int32 `json:"maxDurationSeconds,omitempty"`
	// MaxTokens caps model tokens the run may consume.
	MaxTokens int64 `json:"maxTokens,omitempty"`
	// MaxCostUSD caps the run's spend.
	MaxCostUSD float64 `json:"maxCostUSD,omitempty"`
}

// ---------------------------------------------------------------------------
// Result envelope — returned by a task (the goober completion tool for agentic
// tasks; the runner return for deterministic tasks).
// ---------------------------------------------------------------------------

// ResultStatus is the terminal status of a task.
type ResultStatus string

const (
	// ResultSuccess means the task met its goal.
	ResultSuccess ResultStatus = "success"
	// ResultFailed means the task did not meet its goal.
	ResultFailed ResultStatus = "failed"
	// ResultNeedsEscalation means the task needs human/other intervention.
	ResultNeedsEscalation ResultStatus = "needs-escalation"
)

// ResultEnvelope is the standard task result the engine acts on, and that gates
// and telemetry consume (TSK-Q2). Shape: status, outputs, artifacts, summary,
// metrics, error?.
//
// Runtime wire envelope, not a Kubernetes object — excluded from controller-gen
// DeepCopy generation (its free-form Outputs map cannot be deep-copied).
// +kubebuilder:object:generate=false
type ResultEnvelope struct {
	// Status is the terminal status of the task.
	Status ResultStatus `json:"status"`
	// Outputs are named result values downstream tasks/gates can consume.
	Outputs map[string]interface{} `json:"outputs,omitempty"`
	// Artifacts are produced artifacts (e.g. PR links, files).
	Artifacts []Artifact `json:"artifacts,omitempty"`
	// Summary is a human-readable summary of what happened.
	Summary string `json:"summary,omitempty"`
	// Metrics are numeric measures (duration, tokens, cost, custom).
	Metrics map[string]float64 `json:"metrics,omitempty"`
	// Error carries failure detail; set when status != success.
	Error *ErrorInfo `json:"error,omitempty"`
}

// Artifact is a produced output referenced by URI.
type Artifact struct {
	// Type categorizes the artifact (e.g. "pull-request", "file", "log").
	Type string `json:"type"`
	// URI locates the artifact.
	URI string `json:"uri"`
	// Label is an optional human-facing label.
	Label string `json:"label,omitempty"`
}

// ErrorInfo describes a task failure.
type ErrorInfo struct {
	// Code is a stable, machine-readable error code.
	Code string `json:"code"`
	// Message is the human-readable error message.
	Message string `json:"message"`
	// Retryable indicates whether a retry might succeed (informs WF-021 retries).
	Retryable bool `json:"retryable,omitempty"`
}

// ---------------------------------------------------------------------------
// Verdict — returned by an agentic reviewer gate (mirrors the result envelope).
// ---------------------------------------------------------------------------

// VerdictDecision is the outcome of a reviewer gate.
type VerdictDecision string

const (
	// VerdictPass approves.
	VerdictPass VerdictDecision = "pass"
	// VerdictFail rejects.
	VerdictFail VerdictDecision = "fail"
	// VerdictNeedsChanges requests changes before approval.
	VerdictNeedsChanges VerdictDecision = "needs-changes"
)

// Severity ranks a finding.
type Severity string

// Severity levels for reviewer findings, from least to most serious.
const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityError    Severity = "error"
	SeverityCritical Severity = "critical"
)

// Verdict is the structured result an agentic reviewer gate produces (GT-Q1).
// Shape: decision, findings[], summary.
type Verdict struct {
	// Decision is the reviewer's outcome; the gate maps it to a branch.
	Decision VerdictDecision `json:"decision"`
	// Findings enumerate specific issues the reviewer found.
	Findings []Finding `json:"findings,omitempty"`
	// Summary is a human-readable summary of the review.
	Summary string `json:"summary,omitempty"`
}

// Finding is a single issue raised by a reviewer.
type Finding struct {
	// Severity ranks the finding.
	Severity Severity `json:"severity"`
	// Message describes the issue.
	Message string `json:"message"`
	// Location optionally points at where the issue is (e.g. "path/to/file:42").
	Location string `json:"location,omitempty"`
}

// IsValid reports whether s is a known result status.
func (s ResultStatus) IsValid() bool {
	switch s {
	case ResultSuccess, ResultFailed, ResultNeedsEscalation:
		return true
	}
	return false
}

// IsValid reports whether d is a known verdict decision.
func (d VerdictDecision) IsValid() bool {
	switch d {
	case VerdictPass, VerdictFail, VerdictNeedsChanges:
		return true
	}
	return false
}
