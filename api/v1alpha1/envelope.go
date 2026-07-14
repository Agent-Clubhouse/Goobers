package v1alpha1

// This file defines the V0 stage contract: the three canonical wire envelopes
// every stage executor and the runner exchange at runtime. They are plain Go
// types (JSON-tagged) — NOT CRDs — so the runner, stage executors, providers,
// and gate evaluators can import them without pulling in CRD machinery. JSON
// Schemas in api/schemas/ mirror these shapes and are the cross-language,
// closed (additionalProperties:false) contract. See docs/stage-contract.md.
//
// Terminology: a "stage" (ARCHITECTURE.md §5) is what the workflow/task types
// call a "task"; the terms are equivalent. Field names keep the task-flavored
// spelling (taskId, ...) already used by the workflow definition and compiler.
//
// Load-bearing invariant (ARCHITECTURE.md §2.4): stages communicate ONLY through
// envelopes and artifact pointers. No stage reaches into another stage's state.
// The invocation envelope therefore carries context *pointers* (see ContextPointer
// in artifact.go) — never the result bodies of upstream stages.

// StageContractVersion identifies the version of the stage contract these types
// and the api/schemas/*.schema.json documents implement. The schemas are closed:
// unknown fields are a validation error, and additive changes bump this version.
const StageContractVersion = "v1alpha1"

// ---------------------------------------------------------------------------
// Invocation envelope — what the runner hands a stage when the workflow advances.
// ---------------------------------------------------------------------------

// InvocationEnvelope is the standard context block delivered to a stage at
// invocation (agentic via the harness adapter, or to a deterministic stage
// runner). It carries the goal, the isolated workspace, read-only context
// pointers, the declared capability grants, and the stage's static config.
//
// It deliberately carries NO upstream result bodies: a stage consumes prior work
// only by resolving ContextPointers read-only from the journal (§2.4). This makes
// cross-stage state reach-through impossible by construction.
//
// This is a runtime wire envelope, not a Kubernetes object, so it is excluded
// from controller-gen DeepCopy generation (its free-form Inputs map cannot be
// deep-copied generically).
// +kubebuilder:object:generate=false
type InvocationEnvelope struct {
	// TaskID identifies this stage instance within the run.
	TaskID string `json:"taskId"`
	// WorkflowID identifies the workflow definition being executed.
	WorkflowID string `json:"workflowId"`
	// RunID identifies this run (the OpenTelemetry trace id for the run).
	RunID string `json:"runId"`
	// Gaggle is the gaggle this run belongs to.
	Gaggle string `json:"gaggle"`
	// Goal is the intended outcome of this stage (from the stage definition).
	Goal string `json:"goal"`
	// Workspace is the absolute path to the fresh, isolated, disposable working
	// copy (§5) this stage runs in. The runner guarantees it exists.
	Workspace string `json:"workspace"`
	// RepoRef is the target repository for this run.
	RepoRef RepoRef `json:"repoRef"`
	// Item is the backlog item / trigger payload that started the run. Nil for
	// schedule/signal-triggered runs with no originating item. It is a bounded
	// provider-neutral descriptor, not another stage's state; the authoritative,
	// content-digested snapshot is a ContextPointer into the journal's inputs/.
	Item *BacklogItem `json:"item,omitempty"`
	// ContextPointers are the read-only inputs this stage may consume: journal
	// artifact pointers (upstream outputs, input snapshots) and external refs
	// (e.g. issue/PR URLs). Pointers only — never upstream result bodies.
	ContextPointers []ContextPointer `json:"contextPointers,omitempty"`
	// Capabilities are the capability grants declared by this stage's definition
	// (e.g. "github:issues:write", "repo:push"). Undeclared use fails closed:
	// credentials for capabilities not listed here are never materialized (§5).
	Capabilities []string `json:"capabilities,omitempty"`
	// Limits bound this stage's execution (duration/tokens/cost).
	Limits Limits `json:"limits"`
	// Inputs are the stage's static config from its definition (plus any values
	// the compiler resolved for it). This is the stage's own config, not another
	// stage's runtime state.
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

// Limits bound a stage's execution. Zero values mean "no explicit limit".
type Limits struct {
	// MaxDurationSeconds caps wall-clock time for the stage.
	MaxDurationSeconds int32 `json:"maxDurationSeconds,omitempty"`
	// MaxTokens caps model tokens the run may consume.
	MaxTokens int64 `json:"maxTokens,omitempty"`
	// MaxCostUSD caps the run's spend.
	MaxCostUSD float64 `json:"maxCostUSD,omitempty"`
}

// ---------------------------------------------------------------------------
// Result envelope — what a stage returns.
// ---------------------------------------------------------------------------

// ResultStatus is the terminal status of a stage.
type ResultStatus string

const (
	// ResultSuccess means the stage met its goal; the runner advances.
	ResultSuccess ResultStatus = "success"
	// ResultFailure means the stage did not meet its goal; the runner applies the
	// stage's retry policy and, if exhausted, branches on failure.
	ResultFailure ResultStatus = "failure"
	// ResultBlocked means the stage cannot proceed without external intervention
	// (human input, an unmet dependency); the runner halts the run pending it.
	ResultBlocked ResultStatus = "blocked"
	// ResultNoWork means the stage ran without error but found nothing to act
	// on (issue #233: an empty-backlog claim tick) — distinct from
	// ResultSuccess (which implies the stage actually produced work for a
	// downstream stage to consume) and ResultFailure (which implies a retry
	// policy should apply). The runner short-circuits a ResultNoWork task
	// straight to a clean PhaseCompleted, regardless of the task's declared
	// Next — an agentic downstream stage is never invoked with no subject
	// (ARCHITECTURE.md's "don't fake a success that then does nothing
	// useful" principle). A stage that genuinely errored (a provider/auth
	// failure, a malformed query) must still return ResultFailure, not
	// ResultNoWork — this status is only for "correctly found nothing," the
	// steady state of an idle instance, never a masked error.
	ResultNoWork ResultStatus = "no-work"
)

// ResultEnvelope is the standard stage result the runner acts on, and that gates
// and telemetry consume. Bulk outputs are written into the journal and returned
// as ArtifactPointers; Outputs carries only small declared scalar values.
//
// Runtime wire envelope, not a Kubernetes object — excluded from controller-gen
// DeepCopy generation (its free-form Outputs map cannot be deep-copied).
// +kubebuilder:object:generate=false
type ResultEnvelope struct {
	// Status is the terminal status of the stage.
	Status ResultStatus `json:"status"`
	// Outputs are small, named scalar values downstream stages/gates can consume
	// directly. Anything larger than a scalar is an artifact, referenced by
	// pointer — state does not travel through Outputs.
	Outputs map[string]interface{} `json:"outputs,omitempty"`
	// Artifacts are the stage's produced outputs, each a journal-relative pointer
	// (path + sha256 digest). Downstream stages receive these as ContextPointers.
	Artifacts []ArtifactPointer `json:"artifacts,omitempty"`
	// Summary is a human-readable summary of what happened.
	Summary string `json:"summary,omitempty"`
	// Metrics are numeric measures (duration, tokens, cost, custom).
	Metrics map[string]float64 `json:"metrics,omitempty"`
	// Error carries failure detail; set when status != success.
	Error *ErrorInfo `json:"error,omitempty"`
}

// ErrorInfo describes a stage failure.
type ErrorInfo struct {
	// Code is a stable, machine-readable error code.
	Code string `json:"code"`
	// Message is the human-readable error message.
	Message string `json:"message"`
	// Retryable indicates whether a retry might succeed (informs the runner's
	// retry decision alongside the stage's declared policy).
	Retryable bool `json:"retryable,omitempty"`
}

// ---------------------------------------------------------------------------
// Verdict — what a gate evaluator returns (§5 gates).
// ---------------------------------------------------------------------------

// VerdictDecision is the outcome of a gate evaluator.
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

// Verdict is the structured result a gate evaluator produces. The gate maps the
// decision to a branch. Evidence points at journal artifacts (e.g. a diff, a
// test log) that back the decision — pointers, never inlined state.
type Verdict struct {
	// Decision is the evaluator's outcome; the gate maps it to a branch.
	Decision VerdictDecision `json:"decision"`
	// Rationale explains the decision in prose.
	Rationale string `json:"rationale,omitempty"`
	// Evidence are journal artifact pointers backing the decision.
	Evidence []ArtifactPointer `json:"evidence,omitempty"`
	// Findings enumerate specific issues the evaluator found.
	Findings []Finding `json:"findings,omitempty"`
	// Summary is a human-readable summary of the review.
	Summary string `json:"summary,omitempty"`
}

// Finding is a single issue raised by an evaluator.
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
	case ResultSuccess, ResultFailure, ResultBlocked, ResultNoWork:
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
