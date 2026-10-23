package engine

import "context"

// StartResult reports the outcome of starting a run.
type StartResult struct {
	// RunID is the run/workflow id the run executes under.
	RunID string
	// AlreadyRunning is true when a run with the same id was already claimed,
	// whether it is still running or closed, so this Start was a no-op. This is
	// the engine's exactly-once guarantee: a deterministic RunID makes a
	// duplicate Start idempotent rather than launching a second run.
	AlreadyRunning bool
	// Adopt means the current owner may reconcile a predecessor's accepted start.
	Adopt bool
	// NotMine means the existing execution belongs to another acceptance or newer epoch.
	NotMine bool
}

// StartAdmission is the cloud composition boundary. AdmitStart reads durable
// acceptance custody under the live lease fence; it must source the run ID and
// owner from that custody, never from caller-supplied identity strings.
// ConfirmStart revalidates the fence after Temporal and commits mine/adopt
// reconciliation before callers may perform owned-run effects. On NotMine it
// only revalidates leadership; it must not mutate acceptance or run state.
type StartAdmission interface {
	AdmitStart(context.Context, RunInput) (RunInput, error)
	ConfirmStart(context.Context, RunInput, StartResult) error
}

// Starter begins a durably accepted engine workflow.
type Starter interface {
	Start(context.Context, RunInput) (StartResult, error)
}
