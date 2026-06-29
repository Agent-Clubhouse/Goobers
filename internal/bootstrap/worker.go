package bootstrap

import (
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/invoke"
)

// DefaultTaskQueue is the Temporal task queue the engine worker and the
// scheduler's TemporalStarter agree on.
const DefaultTaskQueue = "goobers-engine"

// EngineDeps are the execution seams a goober runtime provides to the engine
// worker. Goober is required (agentic tasks/reviews); Det and Auto are optional
// (deterministic tasks / automated gates) and may be nil.
type EngineDeps struct {
	Goober invoke.Goober
	Det    invoke.Deterministic
	Auto   invoke.Automated
}

// RegisterEngine registers the engine workflow and its activities (wired to the
// provided runtime seams) on a Temporal worker. The real goober-runtime binary
// and the e2e harness both call this so the worker is identical.
func RegisterEngine(w worker.Worker, deps EngineDeps) {
	engine.RegisterWith(w, &engine.Activities{
		Goober: deps.Goober,
		Det:    deps.Det,
		Auto:   deps.Auto,
	})
}

// NewStarter builds the scheduler's run Starter over a Temporal client and task
// queue. Pass the result as SchedulerDeps.Starter.
func NewStarter(c client.Client, taskQueue string) engine.Starter {
	if taskQueue == "" {
		taskQueue = DefaultTaskQueue
	}
	return engine.NewTemporalStarter(c, taskQueue)
}

// DialTemporal connects to a Temporal frontend. A thin wrapper so the cmd
// entrypoints don't each reimplement client construction.
func DialTemporal(hostPort, namespace string) (client.Client, error) {
	return client.Dial(client.Options{HostPort: hostPort, Namespace: namespace})
}
