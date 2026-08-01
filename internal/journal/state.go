package journal

import "time"

// RunPhase is the coarse lifecycle phase of a run, held in the state.json
// checkpoint so a reader (or a restarting runner) knows at a glance whether a run
// is still live.
type RunPhase string

const (
	// PhaseRunning means the run has not reached a terminal state.
	PhaseRunning RunPhase = "running"
	// PhaseCompleted means the run finished successfully.
	PhaseCompleted RunPhase = "completed"
	// PhaseFailed means the run finished unsuccessfully.
	PhaseFailed RunPhase = "failed"
	// PhaseAborted means the run ended on a defined abort branch.
	PhaseAborted RunPhase = "aborted"
	// PhaseEscalated means the run ended needing human intervention.
	PhaseEscalated RunPhase = "escalated"
)

// State is the atomic checkpoint in state.json: a derived summary the local
// runner replays on restart to resume from the last completed stage. It is
// EXCLUDED from conformance (§3.3) — a convenience for recovery and inspection,
// never the source of truth. The event journal is the source of truth; State is
// always reconstructable from it (see Reader.Recover).
type State struct {
	// Schema is the state.json schema version.
	Schema string `json:"schema"`
	// RunID mirrors the run identity.
	RunID string `json:"runId"`
	// Phase is the coarse lifecycle phase.
	Phase RunPhase `json:"phase"`
	// MachineState is the current workflow state-machine node (the state name
	// the runner resumes at). Empty once the run is terminal.
	//
	// This remains the ROOT cursor and keeps its exact meaning: for every run
	// that never forks it is the whole story, so existing readers are
	// unaffected. When a run is inside a parallel it names the parallel state
	// itself, and the per-branch positions live in Branches.
	MachineState string `json:"machineState,omitempty"`
	// Branches are the per-branch cursors while a run is inside a parallel,
	// in declaration order. Empty for every run that never forks.
	//
	// Like the rest of State this is a DERIVED checkpoint, always
	// reconstructable from the event journal — so adding it is a
	// checkpoint-shape change, not a contract change.
	Branches []BranchCursor `json:"branches,omitempty"`
	// Reason is the human-facing explanation for a terminal run, mirrored
	// from the run.finished event's own Error.Message when the terminal
	// transition carried one (e.g. a WF-016 resume-refusal's digest-mismatch
	// text, #520) — empty for an ordinary business-outcome terminal with no
	// error attached. Durable and grep-able directly in state.json so an
	// operator distinguishes WHY a run stopped without reading the full
	// event log. EXCLUDED from conformance, same as the rest of State.
	Reason string `json:"reason,omitempty"`
	// LastSeq is the seq of the last event durably committed before this
	// checkpoint was written. Recovery uses it to detect events that landed in
	// the journal after the last checkpoint.
	LastSeq uint64 `json:"lastSeq"`
	// UpdatedAt is when this checkpoint was written.
	UpdatedAt time.Time `json:"updatedAt"`
}

// KnownSchema reports whether this checkpoint uses the state.json schema
// version this build owns — the same check Event.KnownSchema applies per
// event, applied here to the single-document state.json (#2054).
func (st State) KnownSchema() bool { return st.Schema == StateSchema }

// BranchCursor is one parallel branch's resume position.
type BranchCursor struct {
	// Branch is the numeric branch id (from 1; 0 is the run's root branch, and
	// the root's position is State.MachineState rather than a cursor here).
	Branch int `json:"branch"`
	// Name is the declared branch name.
	Name string `json:"name"`
	// Parallel is the parallel state this branch belongs to.
	Parallel string `json:"parallel"`
	// MachineState is the state this branch resumes at. Empty once the branch
	// has settled.
	MachineState string `json:"machineState,omitempty"`
	// Status is the branch's terminal status once it has settled; empty while
	// the branch is still running.
	Status BranchStatus `json:"status,omitempty"`
}
