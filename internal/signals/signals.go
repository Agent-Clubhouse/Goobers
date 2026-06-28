// Package signals provides a shared graceful-shutdown context for all Goobers
// control-plane binaries. Every long-running process (operator, scheduler,
// runtime) should derive its root context from SetupSignalContext so that
// SIGINT/SIGTERM trigger an orderly cancellation.
package signals

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// SetupSignalContext returns a context that is cancelled on the first SIGINT or
// SIGTERM. A second signal terminates the process immediately with a non-zero
// exit code, so a wedged shutdown can still be force-killed from the same
// terminal.
//
// The returned stop function releases the signal handler; callers should defer
// it.
func SetupSignalContext() (ctx context.Context, stop func()) {
	ctx, cancel := context.WithCancel(context.Background())

	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-ch
		cancel()
		<-ch
		os.Exit(1)
	}()

	return ctx, func() {
		signal.Stop(ch)
		cancel()
	}
}
