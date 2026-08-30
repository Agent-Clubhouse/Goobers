package runner

import (
	"os"
	"runtime"
	"strings"

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
	// PlacementEnvNamespace is the shared prefix of the family. Like the
	// runner.* event namespace, membership is decided by the PREFIX (see
	// placementDeclared): a variable added to this family later turns
	// placement recording on with no second edit to remember here.
	PlacementEnvNamespace = "GOOBERS_RUNNER_"
	// EnvPlacementNode names the cluster node the process is scheduled on
	// (k8s downward API spec.nodeName). This is the ONLY authority for the
	// placement node field on a self attempt — the process hostname is not one.
	EnvPlacementNode = PlacementEnvNamespace + "NODE"
	// EnvPlacementPod names the pod the process runs in (downward API
	// metadata.name).
	EnvPlacementPod = PlacementEnvNamespace + "POD"
	// EnvPlacementImage is the container image reference the process runs
	// under.
	EnvPlacementImage = PlacementEnvNamespace + "IMAGE"
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
		// Node ONLY from the deployment's downward API. os.Hostname() is not
		// a node name — inside a pod it is the POD name — so it goes to Host,
		// the field whose name is true of it in both places.
		Node:  os.Getenv(EnvPlacementNode),
		Pod:   os.Getenv(EnvPlacementPod),
		Image: os.Getenv(EnvPlacementImage),
	}
	if host, err := os.Hostname(); err == nil {
		p.Host = host
	}
	return p
}

// placementDeclared reports whether this deployment has said ANYTHING about
// placement — a runners: inventory in instance.yaml, or any GOOBERS_RUNNER_*
// identity variable. It is the emission gate for placement provenance
// (goobernetes-architecture.md §11 item 1, zero-declaration invariance): an
// instance that declares no runners and sets no placement env must keep
// writing byte-identical journals, so recording a runner.placement event on
// every stage attempt of every such install is exactly the change §11 item 1
// forbids. Once placement is declared, the provenance is what the operator
// asked for.
func placementDeclared(runnersDeclared bool, environ []string) bool {
	if runnersDeclared {
		return true
	}
	for _, entry := range environ {
		key, value, ok := strings.Cut(entry, "=")
		if ok && value != "" && strings.HasPrefix(key, PlacementEnvNamespace) {
			return true
		}
	}
	return false
}

// recordsPlacement is placementDeclared bound to this runner's configuration
// and the live process environment.
func (r *Runner) recordsPlacement() bool {
	return placementDeclared(r.cfg.RunnersDeclared, os.Environ())
}

// SelfPlacement is selfPlacement, exported for the ENGINE's in-process stage
// arms (#3875, decision 005 / finding 002 plan item E3).
//
// Exported rather than reimplemented because per-attempt placement provenance
// is one event shape with one producer of its payload: the engine walk is
// becoming the single driver for every trigger kind, and a second copy of
// "what does this process know about its substrate" is exactly how the
// runner's and the engine's runner.placement events would start describing the
// same host differently. The parity harness compares the two side by side, so
// the copy would be caught — the point of sharing is that it can never be
// written.
func SelfPlacement() journal.Placement { return selfPlacement() }

// PlacementDeclaredInEnvironment is placementDeclared for a process that
// carries no runners inventory of its own — the goobers-worker executing an
// engine stage in-process.
//
// It is the SAME zero-declaration-invariance gate the local runner applies
// (goobernetes-architecture.md §11 item 1): an untouched single-host install
// that declares no runners and sets no GOOBERS_RUNNER_* identity must keep
// producing byte-identical journals, on the engine path as much as on the
// runner's. A worker holds no instance.yaml runners: block, so the
// environment family is the whole of its declaration.
func PlacementDeclaredInEnvironment() bool { return placementDeclared(false, os.Environ()) }
