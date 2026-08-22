package bootstrap

import (
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

// DefaultTaskQueue is the Temporal task queue the engine worker and the
// scheduler's TemporalStarter agree on.
const DefaultTaskQueue = "goobers-engine"

// EngineDeps are the execution seams a goober runtime provides to the engine
// worker. Goober is required (agentic tasks/reviews); Det and Auto are optional
// (deterministic tasks / automated gates) and may be nil. Workspaces provisions
// each stage attempt's disposable working copy — without it every
// workspace-needing stage fails closed (#621), so any host that dispatches
// real stages must wire one.
type EngineDeps struct {
	Goober     invoke.Goober
	Det        invoke.Deterministic
	Auto       invoke.Automated
	Workspaces engine.WorkspaceProvisioner
	Scrubber   journal.Scrubber
	// Canary is the #2931 fail-closed dispatch canary: the exact-value secret
	// registry (journal.RegistryScrubber) the activities assert serialized
	// dispatch envelopes against before executing a stage. Wire the SAME
	// registry every resolver-issued and credential-plane-minted value is
	// registered with; nil disables the canary.
	Canary journal.Scrubber
}

// RegisterEngine registers the engine workflow and its activities (wired to the
// provided runtime seams and connected Temporal service) on a Temporal worker.
// Every deployable worker entrypoint calls this so the worker is identical.
func RegisterEngine(w worker.Worker, temporalClient client.Client, deps EngineDeps) {
	engine.RegisterWith(w, &engine.Activities{
		Goober:          deps.Goober,
		Det:             deps.Det,
		Auto:            deps.Auto,
		ScheduleService: temporalClient.WorkflowService(),
		Workspaces:      deps.Workspaces,
		Scrubber:        deps.Scrubber,
		Canary:          deps.Canary,
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
