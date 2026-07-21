//go:build windows

package shutdown

import (
	"os"
	"os/signal"
)

// notify registers the windows graceful-shutdown trigger for a console process:
// os.Interrupt (Ctrl+C / Ctrl+Break), the only signal os/signal can observe on
// windows. Windows cannot deliver SIGTERM — there is no cross-process "send
// SIGTERM" — so an external supervisor stops the daemon-as-a-service through the
// Service Control Manager instead, which the winsvc handler routes into the same
// shutdown via Notifier.RequestStop(ReasonService). The two paths converge on
// one cancellation, matching the unix `SIGTERM ⇒ graceful drain ⇒ escalation`
// path.
func notify(ch chan<- os.Signal) {
	signal.Notify(ch, os.Interrupt)
}
