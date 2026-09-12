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

// prepareDaemonReadiness waits for the observation required by inventory before
// publishing the daemon address. The listener already serves startup probes;
// the caller opens the shared readiness and webhook gate only after this returns.
func prepareDaemonReadiness(ctx context.Context, reads *readservice.Local, addressPath, address string, stdout io.Writer) error {
	pf(stdout, "%s startup phase=active-counts status=waiting target=%q\n", startupTimestamp(), "api")
	if err := awaitDaemonActiveCounts(ctx, reads); err != nil {
		return fmt.Errorf("initialize active-run counts: %w", err)
	}
	return publishDaemonAPIAddress(addressPath, address)
}

// daemonReadinessStoppedByShutdown distinguishes cancellation of the daemon's
// root lifecycle from a failure of the readiness observation itself. The
// latter includes the private initialActiveCountsTimeout while the root
// context is still live and must continue to fail startup.
func daemonReadinessStoppedByShutdown(ctx context.Context, err error) bool {
	shutdownErr := ctx.Err()
	if shutdownErr == nil {
		return false
	}
	return errors.Is(err, shutdownErr)
}
