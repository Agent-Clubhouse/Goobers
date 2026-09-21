package main

import (
	"context"
	"time"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/version"
)

// startServiceHealth owns the independent diagnostic transport. Local evidence
// remains available when export is disabled, misconfigured, or unavailable.
func startServiceHealth(ctx context.Context, root string, identity *daemonIdentity, setup *schedulerSetup, inventory recoveryInventorySampler, fleet ...fleetHealthSample) <-chan struct{} {
	return startServiceHealthWithStores(ctx, root, identity, setup, inventory, setup.SecretStores, fleet...)
}

func startServiceHealthWithStores(ctx context.Context, root string, identity *daemonIdentity, setup *schedulerSetup, inventory recoveryInventorySampler, stores credentials.StoreResolver, fleet ...fleetHealthSample) <-chan struct{} {
	done := make(chan struct{})
	// Keep local observations and scheduler startup independent of credential
	// resolution. The startup observation waits in this single-record handoff;
	// initialization is bounded well below the six-hour observation interval.
	records := make(chan telemetry.DiagnosticRecord, 1)
	go func() {
		defer close(records)
		sink := func(event journal.Event) { records <- serviceHealthDiagnosticRecord(event) }
		emitServiceHealth(ctx, root, identity, setup.InstanceLog, inventory, serviceHealthInterval, nil, nil, sink)
	}()
	go func() {
		defer close(done)
		initialize, cancel := context.WithTimeout(ctx, 5*time.Second)
		exporter, err := buildDiagnosticExporterWithStores(initialize, setup, stores)
		cancel()
		if err != nil {
			setup.InstanceLog.AppendBestEffort(journal.Event{Type: journal.EventError, Error: &journal.ErrorDetail{Code: "diagnostics_export_unavailable", Message: err.Error()}})
		}
		runHealthExports(ctx, setup, exporter, records, fleet)

		if exporter != nil {
			flush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = exporter.Shutdown(flush)
			stats := exporter.Stats()
			setup.InstanceLog.AppendBestEffort(journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": "diagnostics-export-summary", "accepted": stats.Accepted, "delivered": stats.Delivered, "dropped": stats.Dropped, "failures": stats.Failures}})
		}
	}()
	return done
}

func buildDiagnosticExporterWithStores(ctx context.Context, setup *schedulerSetup, stores credentials.StoreResolver) (*telemetry.DiagnosticExporter, error) {
	otlp := setup.Config.DiagnosticOTLP()
	if !otlp.Enabled() {
		return nil, nil
	}
	cfg := telemetry.Config{
		ServiceVersion: version.Get().Version, BuildCommit: version.Get().Commit,
		Scrubber: journal.Chain(setup.SharedRegistry, journal.NewPatternScrubber()),
	}
	if err := configureOTLP(ctx, &cfg, otlp, setup.SharedRegistry, stores); err != nil {
		return nil, err
	}
	return telemetry.NewDiagnosticExporter(cfg)
}

// Whitelist the public operational contract rather than exporting the instance
// journal wholesale. Paths, raw errors, credentials and arbitrary payloads never
// enter this record. Machine/account identity is exported only with explicit
// diagnostic endpoint consent, never through the journal collector.
func serviceHealthDiagnosticRecord(event journal.Event) telemetry.DiagnosticRecord {
	attrs := make(map[string]any)
	for _, key := range []string{"schemaVersion", "observedAt", "instanceId", "instanceDisplayName", "identityProblem", "machineName", "accountName", "windowCoverage", "daemonStartedAt", "processUptimeSeconds", "observedUncleanRestarts", "observationWindowStart"} {
		if value, ok := event.Runner[key]; ok {
			attrs[key] = value
		}
	}
	if inventory, ok := event.Runner["recoveryInventory"].(map[string]any); ok {
		for _, key := range []string{"state", "used", "limit", "unreadable", "highWaterPercent", "earliestRetainUntil"} {
			if value, ok := inventory[key]; ok {
				attrs["recoveryInventory."+key] = value
			}
		}
	}
	return telemetry.DiagnosticRecord{Time: event.Time, Name: "goobers.service.health", Attributes: attrs}
}

func runHealthExports(ctx context.Context, setup *schedulerSetup, exporter *telemetry.DiagnosticExporter, records <-chan telemetry.DiagnosticRecord, fleet []fleetHealthSample) {
	var ticks <-chan time.Time
	if len(fleet) > 0 {
		ticker := time.NewTicker(setup.Config.Telemetry.Diagnostics.HeartbeatPeriod())
		defer ticker.Stop()
		ticks = ticker.C
		emitFleetHealth(ctx, setup, exporter, fleet, time.Now().UTC())
	}
	for {
		select {
		case record, ok := <-records:
			if !ok {
				return
			}
			if exporter != nil {
				exporter.Emit(record)
			}
		case now := <-ticks:
			emitFleetHealth(ctx, setup, exporter, fleet, now.UTC())
		}
	}
}

func emitFleetHealth(ctx context.Context, setup *schedulerSetup, exporter *telemetry.DiagnosticExporter, fleet []fleetHealthSample, now time.Time) {
	sampleCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, sample := range fleet {
		for _, record := range sample(sampleCtx, now) {
			setup.InstanceLog.AppendBestEffort(journal.Event{Time: record.Time, Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": record.Name, "diagnostic": record.Attributes}})
			if exporter != nil {
				exporter.Emit(record)
			}
		}
	}
}

// startDaemonHealth starts while startup is still in progress; its deferred
// stop joins observers before the read service and instance journal close.
func startDaemonHealth(ctx context.Context, root string, identity *daemonIdentity, setup *schedulerSetup, inventory recoveryInventorySampler, reader fleetHealthReader, ready func() bool) func() {
	healthCtx, cancel := context.WithCancel(ctx)
	done := startServiceHealth(healthCtx, root, identity, setup, inventory, newFleetHealthSampler(root, identity, setup.Config.Telemetry.Diagnostics, reader, ready))
	return func() { cancel(); <-done }
}
