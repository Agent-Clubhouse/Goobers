package main

import (
	"io"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
)

// daemonStartupSetupOptions keeps the startup-only wiring together. Recording
// and accounting stay active; only background Azure uploads wait for release.
// Failed startup leaves the signal unreleased and exporter shutdown cancels its
// waiters. Retrying setup shares this daemon's signal, not any process-global one.
func daemonStartupSetupOptions(notifications notifyFlag, stdout, stderr io.Writer, recovery *localscheduler.RecoveryGate) ([]schedulerSetupOption, func()) {
	started := time.Now()
	replayStart := make(chan struct{})
	return []schedulerSetupOption{
		withDesktopNotifications(notifications, stderr),
		withStartupProgress(newSchedulerSetupProgress(stdout, started, time.Now)),
		withClaimRecoveryGate(recovery),
		withTelemetryReplayStart(replayStart),
	}, sync.OnceFunc(func() { close(replayStart) })
}

// withTelemetryReplayStart defers background Azure replay until the owning
// daemon is ready. It does not delay local recording or durable admission.
// Leaving it nil preserves one-shot command behavior. Failed setup closes its
// exporters without waiting for the signal; the bounded final drain still runs.
func withTelemetryReplayStart(start <-chan struct{}) schedulerSetupOption {
	return func(options *schedulerSetupOptions) {
		options.telemetryReplayStart = start
	}
}
