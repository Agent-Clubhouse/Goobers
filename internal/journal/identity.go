package journal

import "time"

// PinnedWorkflowGraphInputName is the immutable input snapshot containing the
// canonical graph a run started with.
const PinnedWorkflowGraphInputName = "workflow-graph"

// TriggerKind is how a run was started.
type TriggerKind string

const (
	// TriggerManual is `goobers run <workflow>`.
	TriggerManual TriggerKind = "manual"
	// TriggerSchedule is a cron trigger fired by the scheduler.
	TriggerSchedule TriggerKind = "schedule"
	// TriggerSignal is an external signal (e.g. a webhook).
	TriggerSignal TriggerKind = "signal"
	// TriggerItem is a backlog item claimed for work.
	TriggerItem TriggerKind = "item"
)

// Trigger describes what caused a run to start.
type Trigger struct {
	Kind TriggerKind `json:"kind"`
	// Ref is the trigger-specific reference: a cron expression, signal name, or
	// backlog item id. Empty for a bare manual run.
	Ref string `json:"ref,omitempty"`
}

// InputRef names an immutable input snapshot stored under inputs/.
type InputRef struct {
	Name string `json:"name"`
	Ref  Ref    `json:"ref"`
}

// RunIdentity is the pinned identity of a run, serialized to run.yaml. It is
// written once at Create and never edited: a run records the workflow definition
// version it started on and completes on that version (WF-016). Input snapshots
// taken at Create are listed here by content digest.
type RunIdentity struct {
	// Schema is the run.yaml schema version.
	Schema string `json:"schema"`
	// RunID is the run identifier — the OpenTelemetry trace id for the run.
	RunID string `json:"runId"`
	// Workflow is the workflow definition name.
	Workflow string `json:"workflow"`
	// WorkflowVersion is the pinned definition version (WF-016).
	WorkflowVersion int `json:"workflowVersion"`
	// WorkflowDigest is the content digest of the pinned workflow Definition
	// ("sha256:<hex>", from the #9 compiler's Machine.Digest()) — the
	// tamper-evident WF-016 pin: a run starts and completes on this exact
	// definition (ARCHITECTURE.md §4). Optional so runs predating a compiled
	// digest still validate.
	WorkflowDigest string `json:"workflowDigest,omitempty"`
	// GooberDigest is the content digest of the participating resolved goobers:
	// instruction content, skills, model, and harness configuration. Optional
	// for runs created before this pin was introduced.
	GooberDigest string `json:"gooberDigest,omitempty"`
	// Gaggle is the gaggle this run belongs to.
	Gaggle string `json:"gaggle"`
	// Trigger is what started the run.
	Trigger Trigger `json:"trigger"`
	// Inputs are the content-digested input snapshots pinned at run start.
	Inputs []InputRef `json:"inputs,omitempty"`
	// StartedAt is when the run was created. Informational (not conformance).
	StartedAt time.Time `json:"startedAt"`
}
