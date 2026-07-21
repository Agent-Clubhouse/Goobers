//go:build unix

package shutdown

import (
	"os"
	"os/signal"
	"syscall"
)

// notify registers the unix graceful-shutdown signals: SIGINT (Ctrl+C) and
// SIGTERM (the default `kill` / systemd stop). This is the exact behavior
// internal/signals has always had on darwin/linux — the move here is a pure
// refactor so SIGTERM, which windows cannot deliver, stays out of the shared
// code and off the windows build.
func notify(ch chan<- os.Signal) {
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
}
