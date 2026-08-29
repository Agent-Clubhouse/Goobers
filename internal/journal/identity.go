package journal

import (
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// PinnedWorkflowGraphInputName is the immutable input snapshot containing the
// canonical graph a run started with.
const PinnedWorkflowGraphInputName = "workflow-graph"

// PinnedWorkflowDefinitionInputName is the immutable workflow Definition
// snapshot used to reconstruct the exact compiled machine after process loss.
const PinnedWorkflowDefinitionInputName = "workflow-definition"

// PinnedGateGooberCapabilitiesInputName is the immutable snapshot of the
// reviewer-goober capability map pinned into the run at start (#294): an
// agentic gate's reviewer capabilities are instance policy, not part of the
// pinned workflow definition, so post-start consumers (the daemon credential
// plane, PR #3528) must read them from this snapshot rather than the
// currently-served config — otherwise a config edit after run start would
// change a live run's reviewer grants, contradicting WF-016's pinning
// guarantee. The payload is a JSON map[gooberName][]capability.
const PinnedGateGooberCapabilitiesInputName = "gate-goober-capabilities"

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

// RunDriver names the component that walks a run's stages.
type RunDriver string

// DriverEngine marks a run whose walk the tier-3 engine owns on Temporal. The
// engine's own run journal writes it at creation; every other writer leaves
// Driver empty, which means the daemon's in-process runner.
//
// The distinction is load-bearing rather than informational: a daemon that
// restarts mid-run scans its runs tree and resumes anything still
// journal.PhaseRunning, and every WF-016 pin an engine-authored journal
// carries passes that scan's checks — so without this marker a goobers-api
// restart re-drives an engine run in-process while the worker keeps driving
// the same run on Temporal (decision 003, "Phase-0 engine-start hygiene").
// The same applies to the stall sweep, which would terminalize the journal of
// a workflow nothing ever cancelled, and to the operator paths (run
// cancel/abort, HITL resume) that edit a journal the engine still owns.
//
// It is deliberately NOT derived from livejournal.Authored: authorship says
// which writer put the bytes on disk, and under the planned
// livejournal.Writer.Adopt a runner-driven run's journal carries live emit
// keys too. Drivership is a different question and is pinned at creation.
const DriverEngine RunDriver = "engine"

// Trigger describes what caused a run to start.
type Trigger struct {
	Kind TriggerKind `json:"kind"`
	// Ref is the trigger-specific reference: a cron expression, signal name, or
	// backlog item id. Empty for a bare manual run.
	Ref string `json:"ref,omitempty"`
}

// InputRef names an immutable input snapshot stored under inputs/.
type InputRef struct {
	Name      string          `json:"name"`
	Ref       Ref             `json:"ref"`
	Source    string          `json:"source,omitempty"`
	Integrity apiv1.Integrity `json:"integrity"`
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
	// Driver names the component walking this run's stages. Empty — the only
	// value any run.yaml written before this field existed can carry — means
	// the daemon's in-process runner, so every existing journal keeps both
	// its exact bytes and its exact meaning.
	Driver RunDriver `json:"driver,omitempty"`
	// RunControls pins the effective inherited safety budgets this run started
	// with. Nil identifies a legacy run that predates run-control pinning.
	RunControls *apiv1.RunControls `json:"runControls,omitempty"`
	// Placements pins each stage's resolved execution placement at run start
	// (decision 003 ruling 1): the daemon's runner writes the run-start solve's
	// answer here so a resume restores the same pins. Nil — the only value any
	// run.yaml written before this field existed can carry, and what every
	// zero-declaration or local-mode instance still writes — means no stage
	// was placed and every stage takes the self path. Non-normative, like
	// RunControls beside it: never conformance surface. See placementpin.go.
	Placements []PinnedPlacement `json:"placements,omitempty"`
	// Trigger is what started the run.
	Trigger Trigger `json:"trigger"`
	// Inputs are the content-digested input snapshots pinned at run start.
	Inputs []InputRef `json:"inputs,omitempty"`
	// ContinuedFromRunID links this run to the terminal run it continues.
	ContinuedFromRunID string `json:"continuedFromRunId,omitempty"`
	// SourceTerminalSeq is the terminal event generation selected by the
	// operator when this continuation was created.
	SourceTerminalSeq uint64 `json:"sourceTerminalSeq,omitempty"`
	// Operator and RequestedTarget are immutable continuation provenance.
	Operator        string `json:"operator,omitempty"`
	RequestedTarget string `json:"requestedTarget,omitempty"`
	// WorkspaceBranch is the repository branch whose state this run executes.
	// Continuations retain the source branch instead of creating a new run branch.
	WorkspaceBranch string `json:"workspaceBranch,omitempty"`
	// WorkspaceBranchSHA is the commit observed for WorkspaceBranch at creation.
	WorkspaceBranchSHA string `json:"workspaceBranchSha,omitempty"`
	// WorkspaceRepository identifies the repository containing WorkspaceBranch.
	WorkspaceRepository *apiv1.RepoRef `json:"workspaceRepository,omitempty"`
	// ContextPointers are the explicitly admitted cross-run and injected inputs
	// available to a continuation. Ambient source-run context is never copied.
	ContextPointers []apiv1.ContextPointer `json:"contextPointers,omitempty"`
	// StartedAt is when the run was created and anchors maxRunDuration.
	StartedAt time.Time `json:"startedAt"`
}

// KnownSchema reports whether this identity uses the run.yaml schema version
// this build owns — the same check Event.KnownSchema applies per event,
// applied here to the single-document run.yaml (#2054).
func (id RunIdentity) KnownSchema() bool { return id.Schema == RunSchema }

// EngineDriven reports whether the tier-3 engine, rather than the daemon's
// own runner, owns this run's walk. It is the single predicate the daemon's
// resume scan, stall sweep and operator paths consult before acting on a run
// they did not start.
func (id RunIdentity) EngineDriven() bool { return id.Driver == DriverEngine }
