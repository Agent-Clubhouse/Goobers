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
// in a recent log window, and logs its resolution once. Worker logs are
// otherwise clean in that state: a versioned poll that is never routed
// anything succeeds, so the engine stalls with nothing reported (#5950). The
// rule is shared with `goobers status` and `goobers doctor` (#6020).
func (h *Host) checkRoutingOnce(ctx context.Context, c client.Client, reported string) string {
	callCtx, cancel := context.WithTimeout(ctx, routingCheckTimeout)
	routing, err := h.describeRouting(callCtx, c)
	cancel()
	if ctx.Err() != nil {
		return reported
	}
	finding := workerrouting.Check(h.versioned(), h.cfg.BuildVersion, routing, err)
	h.routed.Store(finding.Problem == "")
	h.draining.Store(finding.Draining)
	// Temporal Worker Controller scales a version down only after it is
	// drained. Readiness keeps a still-draining version protected meanwhile.
	switch {
	case finding.Problem != "":
		h.logf("goobers-cloud worker: error: %s", finding.Problem)
	case reported != "" && finding.Draining:
		h.logf("goobers-cloud worker: worker deployment %q replaced this worker's version; it is draining", DeploymentName)
	case reported != "":
		h.logf("goobers-cloud worker: worker deployment %q now routes tasks to this worker", DeploymentName)
	}
	return finding.Problem
}

func (h *Host) logf(format string, args ...any) {
	if h.cfg.Logf != nil {
		h.cfg.Logf(format, args...)
		return
	}
	_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)
}
