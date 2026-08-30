package engine

import "go.temporal.io/sdk/worker"

// RegisterWith registers the engine workflow and its activities on a Temporal
// worker. The runtime (M8) constructs Activities with a real GooberInvoker (and
// optionally deterministic/automated seams) and calls this to make runs
// executable on a task queue.
func RegisterWith(w worker.Worker, a *Activities) {
	w.RegisterWorkflow(Run)
	w.RegisterWorkflow(RunScheduled)
	w.RegisterWorkflow(ClaimScheduled)
	w.RegisterWorkflow(ReconcileSchedules)
	// DispatchOne is registered on the SAME worker as the rest (decision 003
	// ruling 2: "goobers-worker polls the workflow queue and every dispatch
	// queue exactly as today"). It must be registered wherever a caller might
	// start it, and registering it here — rather than at a new call site —
	// is what keeps the worker's registration one list: an unregistered
	// workflow type fails the start with a task-timeout that names nothing
	// useful.
	w.RegisterWorkflow(DispatchOne)
	w.RegisterActivity(a)
}
