package runner

import (
	"os"
	"runtime"

	"github.com/goobers/goobers/internal/journal"
)

// Placement identity environment variables. They cover the one gap
// self-observation cannot: a containerized daemon (mode 2) cannot see its own
// image reference or cluster node from inside the container, so the
// deployment supplies them (downward API for the node, the pod spec for the
// image). A bare host leaves them unset and the fields stay absent. The
// mode-3 dispatcher (#3513) does NOT ride these: it creates the pod, so it
// fills the same journal.Placement fields directly — one event shape, one
// emission mechanism (goobernetes-architecture.md §7).
const (
	// EnvPlacementNode names the cluster node the process is scheduled on
	// (k8s downward API spec.nodeName).
	EnvPlacementNode = "GOOBERS_RUNNER_NODE"
	// EnvPlacementPod names the pod the process runs in (downward API
	// metadata.name).
	EnvPlacementPod = "GOOBERS_RUNNER_POD"
	// EnvPlacementImage is the container image reference the process runs
	// under.
	EnvPlacementImage = "GOOBERS_RUNNER_IMAGE"
)

// selfPlacement captures what THIS process knows about the substrate a stage
// attempt executes on. In modes 1–2 the attempt runs in the daemon's own
// process, so the placement is the runners inventory's implicit "self" entry:
// the process's GOOS, its host (or the deployment-declared node), and — when
// the deployment says so — its pod and image identity. No queue-wait or
// pod-start timestamps: a self attempt never queued, and recording only what
// the substrate knows is the Placement contract.
func selfPlacement() journal.Placement {
	p := journal.Placement{
		Runner: journal.PlacementRunnerSelf,
		OS:     runtime.GOOS,
		Node:   os.Getenv(EnvPlacementNode),
		Pod:    os.Getenv(EnvPlacementPod),
		Image:  os.Getenv(EnvPlacementImage),
	}
	if p.Node == "" {
		if host, err := os.Hostname(); err == nil {
			p.Node = host
		}
	}
	return p
}
