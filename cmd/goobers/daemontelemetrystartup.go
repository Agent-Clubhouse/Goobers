package main

// withTelemetryReplayStart defers background Azure replay until the owning
// daemon is ready. It does not delay local recording or durable admission.
// Leaving it nil preserves one-shot command behavior. Failed setup closes its
// exporters without waiting for the signal; the bounded final drain still runs.
func withTelemetryReplayStart(start <-chan struct{}) schedulerSetupOption {
	return func(options *schedulerSetupOptions) {
		options.telemetryReplayStart = start
	}
}
