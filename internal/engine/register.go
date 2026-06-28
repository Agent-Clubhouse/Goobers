package engine

import "go.temporal.io/sdk/worker"

// RegisterWith registers the engine workflow and its activities on a Temporal
// worker. The runtime (M8) constructs Activities with a real GooberInvoker (and
// optionally deterministic/automated seams) and calls this to make runs
// executable on a task queue.
func RegisterWith(w worker.Worker, a *Activities) {
	w.RegisterWorkflow(Run)
	w.RegisterActivity(a)
}
