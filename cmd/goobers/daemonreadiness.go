package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/goobers/goobers/internal/readservice"
)

var newDaemonReadService = readservice.NewLocal

// Startup may wait for a background observation; HTTP handlers never do. This
// bound also keeps an unresponsive projection from indefinitely blocking startup.
const initialActiveCountsTimeout = 30 * time.Second

func awaitDaemonActiveCounts(ctx context.Context, reads *readservice.Local) error {
	sampleCtx, cancel := context.WithTimeout(ctx, initialActiveCountsTimeout)
	defer cancel()
	return reads.WaitForInitialActiveRunSample(sampleCtx)
}

// prepareDaemonReadiness waits for the observation required by inventory.
// The listener and its published address are already available for startup
// probes; the caller opens the shared readiness and webhook gate afterward.
func prepareDaemonReadiness(ctx context.Context, reads *readservice.Local, tracker *startupPhaseTracker, stdout io.Writer) error {
	return runStartupPhase(stdout, tracker, "active-counts", "api", func() error {
		if err := awaitDaemonActiveCounts(ctx, reads); err != nil {
			return fmt.Errorf("initialize active-run counts: %w", err)
		}
		return nil
	})
}

// daemonStartupStoppedByShutdown distinguishes cancellation of the daemon's
// root lifecycle from a failure of a startup operation itself. A private
// operation timeout while the root context is still live, or an unrelated
// error that happens while shutdown is also in progress, must continue to fail
// startup.
func daemonStartupStoppedByShutdown(ctx context.Context, err error) bool {
	shutdownErr := ctx.Err()
	if shutdownErr == nil {
		return false
	}
	return errors.Is(err, shutdownErr)
}

// daemonStartupFailure turns a failed startup operation into the daemon's
// documented exit status. Keeping this classification beside the predicate
// prevents each startup phase from growing its own cancellation branch while
// still leaving phase-specific diagnostics at the call site.
func daemonStartupFailure(ctx context.Context, err error, report func()) int {
	if daemonStartupStoppedByShutdown(ctx, err) {
		return 0
	}
	report()
	return 1
}

// daemonStartupWarning reports a degraded startup operation, unless the
// operation stopped because daemon shutdown was already requested. The return
// value tells the caller to stop startup cleanly in that latter case.
func daemonStartupWarning(ctx context.Context, err error, report func()) bool {
	if err == nil {
		return false
	}
	if daemonStartupStoppedByShutdown(ctx, err) {
		return true
	}
	report()
	return false
}
