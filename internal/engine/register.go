package engine

import (
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// RegisterWith registers the engine workflow and its activities on a Temporal
// worker. The runtime (M8) constructs Activities with a real GooberInvoker (and
// optionally deterministic/automated seams) and calls this to make runs
// executable on a task queue.
func RegisterWith(w worker.Worker, a *Activities) {
	versioned := workflow.RegisterOptions{VersioningBehavior: workflow.VersioningBehaviorPinned}
	w.RegisterWorkflowWithOptions(Run, versioned)
	w.RegisterWorkflowWithOptions(RunScheduled, versioned)
	w.RegisterWorkflowWithOptions(ClaimScheduled, versioned)
	w.RegisterWorkflowWithOptions(ReconcileSchedules, versioned)
	// DispatchOne is registered on the SAME worker as the rest (decision 003
	// ruling 2: "goobers-worker polls the workflow queue and every dispatch
	// queue exactly as today"). It must be registered wherever a caller might
	// start it, and registering it here — rather than at a new call site —
	// is what keeps the worker's registration one list: an unregistered
	// workflow type fails the start with a task-timeout that names nothing
	// useful.
	w.RegisterWorkflowWithOptions(DispatchOne, versioned)
	w.RegisterActivity(a)
}
