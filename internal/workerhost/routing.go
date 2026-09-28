package workerhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

// DeploymentName is the Temporal worker deployment a versioned worker
// registers under.
const DeploymentName = "goobers"

const (
	// routingCheckDelay lets the pollers register their version before the
	// first check, so a fresh rollout is not reported as a mismatch.
	routingCheckDelay = 30 * time.Second
	// routingCheckInterval re-checks for the life of the process: the current
	// version is changed out-of-band by operators, not by this worker.
	routingCheckInterval = 5 * time.Minute
	routingCheckTimeout  = 10 * time.Second
)

// describeDeploymentRouting reads the "goobers" worker deployment's routing
// config: which version receives new workflows and the tasks of unpinned
// ones.
func describeDeploymentRouting(ctx context.Context, c client.Client) (client.WorkerDeploymentRoutingConfig, error) {
	if c == nil {
		return client.WorkerDeploymentRoutingConfig{}, errors.New("no temporal client")
	}
	resp, err := c.WorkerDeploymentClient().GetHandle(DeploymentName).Describe(ctx, client.WorkerDeploymentDescribeOptions{})
	if err != nil {
		return client.WorkerDeploymentRoutingConfig{}, err
	}
	return resp.Info.RoutingConfig, nil
}

// routingProblem explains why the deployment's routing sends this worker no
// tasks, or returns "" when it does not (#5950). Worker logs are otherwise
// clean in that state: a versioned poll that is never routed anything
// succeeds, so the engine stalls with nothing reported.
//
// A describe error is a problem only for a versioned worker. An unversioned
// worker on a namespace with versioning disabled, or where no versioned worker
// ever registered, cannot describe the deployment and needs nothing from it.
func routingProblem(versioned bool, buildID string, routing client.WorkerDeploymentRoutingConfig, describeErr error) string {
	if !versioned {
		if describeErr != nil || routing.CurrentVersion == nil {
			return ""
		}
		return fmt.Sprintf("worker deployment %q current version is %s, so this UNVERSIONED worker receives no new workflow tasks "+
			"(engine.workerVersioning is off). Either set engine.workerVersioning: true, or point the deployment at unversioned workers: "+
			"temporal worker deployment set-current-version --deployment-name %s --unversioned",
			DeploymentName, versionLabel(routing.CurrentVersion), DeploymentName)
	}
	if describeErr != nil {
		return fmt.Sprintf("cannot verify that worker deployment %q routes tasks to this worker (build %s): %v",
			DeploymentName, buildID, describeErr)
	}
	if servesVersion(routing.CurrentVersion, buildID) {
		return ""
	}
	if servesVersion(routing.RampingVersion, buildID) && routing.RampingVersionPercentage > 0 {
		return ""
	}
	return fmt.Sprintf("worker deployment %q current version is %s but this worker serves %s.%s, so it receives no new workflow tasks "+
		"and the engine stalls. Make this build current: "+
		"temporal worker deployment set-current-version --deployment-name %s --build-id %s",
		DeploymentName, versionLabel(routing.CurrentVersion), DeploymentName, buildID, DeploymentName, buildID)
}

func servesVersion(v *worker.WorkerDeploymentVersion, buildID string) bool {
	return v != nil && v.DeploymentName == DeploymentName && v.BuildID == buildID
}

func versionLabel(v *worker.WorkerDeploymentVersion) string {
	if v == nil {
		return "__unversioned__"
	}
	return v.DeploymentName + "." + v.BuildID
}

// checkRoutingLoop reports routing problems until ctx ends. It never fails the
// worker: a mismatch is an operator action away from healthy, and a restart
// would not change it.
func (h *Host) checkRoutingLoop(ctx context.Context, c client.Client) {
	timer := time.NewTimer(h.routingDelay)
	defer timer.Stop()
	reported := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		reported = h.checkRoutingOnce(ctx, c, reported)
		timer.Reset(h.routingInterval)
	}
}

// checkRoutingOnce logs a current problem on every check, so it stays visible
// in a recent log window, and logs its resolution once.
func (h *Host) checkRoutingOnce(ctx context.Context, c client.Client, reported string) string {
	callCtx, cancel := context.WithTimeout(ctx, routingCheckTimeout)
	routing, err := h.describeRouting(callCtx, c)
	cancel()
	if ctx.Err() != nil {
		return reported
	}
	problem := routingProblem(h.versioned(), h.cfg.BuildVersion, routing, err)
	switch {
	case problem != "":
		h.logf("goobers worker: error: %s", problem)
	case reported != "":
		h.logf("goobers worker: worker deployment %q now routes tasks to this worker", DeploymentName)
	}
	return problem
}

func (h *Host) logf(format string, args ...any) {
	if h.cfg.Logf != nil {
		h.cfg.Logf(format, args...)
		return
	}
	_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)
}
