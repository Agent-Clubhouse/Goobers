// Package signals provides a shared graceful-shutdown context for all Goobers
// control-plane binaries. Every long-running process (operator, scheduler,
// runtime) should derive its root context from SetupSignalContext so that a
// platform shutdown trigger (unix SIGINT/SIGTERM, windows Ctrl+C) triggers an
// orderly cancellation.
//
// The cross-platform trigger mechanism itself — including the exported
// service-stop hook the Windows SCM handler drives and the shared
// drain-then-escalate sequence — lives in internal/platform/shutdown, alongside
// the internal/platform/{lock,proc} seams. This package is the thin, stable
// entry point the binaries already import; it delegates to that seam.
package signals

import (
	"context"

	"github.com/goobers/goobers/internal/platform/shutdown"
)

// SetupSignalContext returns a context that is cancelled on the first OS
// shutdown signal. A second signal terminates the process immediately with a
// non-zero exit code, so a wedged shutdown can still be force-killed from the
// same terminal.
//
// The returned stop function releases the signal handler and cancels the
// context; callers should defer it. Behavior on darwin/linux is exactly as
// before: SIGINT and SIGTERM both trigger the graceful path.
func SetupSignalContext() (ctx context.Context, stop func()) {
	n := shutdown.Notify()
	return n.Context(), n.Stop
}
