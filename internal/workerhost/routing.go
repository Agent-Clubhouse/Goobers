package workerhost

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/goobers/goobers/internal/workerrouting"
)

// DeploymentName is the Temporal worker deployment a versioned worker
// registers under.
const DeploymentName = workerrouting.DeploymentName

const (
	// routingCheckDelay lets the pollers register their version before the
	// first check, so a fresh rollout is not reported as a mismatch.
	routingCheckDelay = 30 * time.Second
	// routingCheckInterval re-checks for the life of the process: the current
	// version is changed out-of-band by operators, not by this worker.
	routingCheckInterval = 5 * time.Minute
	routingCheckTimeout  = 10 * time.Second
)

// routingProblem explains why the deployment's routing sends this worker no
// tasks, or returns "" when it does not (#5950). Worker logs are otherwise
// clean in that state: a versioned poll that is never routed anything
// succeeds, so the engine stalls with nothing reported. The rule is shared
// with `goobers status` and `goobers doctor` (#6020).
func routingProblem(versioned bool, buildID string, routing client.WorkerDeploymentRoutingConfig, describeErr error) string {
	return workerrouting.Check(versioned, buildID, routing, describeErr).Problem
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
