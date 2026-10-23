package main

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/version"
)

// buildTelemetryClient constructs the OTel client that spans the runner walk
// (run/task/gate) and scheduler decisions, writing completed spans under
// RunsDir via JournalSpanExporter (issue #126) — the same run journal
// layout goobers trace/telemetry read back through the rollup. Shared by
// up.go/run.go exactly like buildRunnerConfig; each caller owns calling
// Shutdown on the returned client once it's done driving runs.
func buildTelemetryClient(
	ctx context.Context,
	l instance.Layout,
	scrubber journal.Scrubber,
	registry *journal.RegistryScrubber,
	telemetryConfig instance.TelemetryConfig,
	stores credentials.StoreResolver,
	exporterHealth *telemetry.ExporterHealth,
	// Nil keeps one-shot setup immediate; the daemon supplies its readiness signal.
	replayStart <-chan struct{},
) (*telemetry.Client, error) {
	cfg := telemetry.Config{
		ServiceName:             "goobers",
		ServiceVersion:          version.Get().Version,
		BuildCommit:             version.Get().Commit,
		SpanExporter:            telemetry.NewPerGaggleJournalSpanExporter(l.Root, scrubber),
		Scrubber:                scrubber,
		Batch:                   true,
		JournalRoot:             l.Root,
		AzureMonitorReplayStart: replayStart,
		ExporterHealth:          exporterHealth,
	}
	// Only the durable identity is trustworthy; legacy roots remain unidentified.
	// Carry it on every signal as a resource attribute so operators can correlate
	// process restarts with the same customer-managed instance and its journals.
	cfg.JournalInstanceID, cfg.ResourceAttributes = telemetryInstanceIdentities(l.Root)
	if telemetryConfig.OTLP != nil {
		if err := runtimewiring.ConfigureOTLP(ctx, &cfg, *telemetryConfig.OTLP, registry, stores); err != nil {
			return nil, err
		}
	}
	if telemetryConfig.AzureMonitor != nil {
		if err := runtimewiring.ConfigureAzureMonitor(ctx, &cfg, *telemetryConfig.AzureMonitor, telemetryConfig.EffectiveCollectionProfile(), l.Root, registry, stores); err != nil {
			return nil, err
		}
	}
	// telemetry.New may return a non-nil *Client alongside an error wrapping
	// telemetry.ErrOTLPUnavailable (invalid TLS material) — that Client is
	// still usable for local-only telemetry, so callers must not treat every
	// non-nil error here as a construction failure. See daemon.go's call
	// site for the degrade handling.
	namedErr := runtimewiring.ConfigureNamedTelemetry(ctx, &cfg, telemetryConfig, l.Root, registry, stores, false)
	client, err := telemetry.New(ctx, cfg)
	return client, errors.Join(namedErr, err)
}
