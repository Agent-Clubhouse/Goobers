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
	MachineState string `json:"machineState,omitempty"`
	// LastSeq is the seq of the last event durably committed before this
	// checkpoint was written. Recovery uses it to detect events that landed in
	// the journal after the last checkpoint.
	LastSeq uint64 `json:"lastSeq"`
	// UpdatedAt is when this checkpoint was written.
	UpdatedAt time.Time `json:"updatedAt"`
}
