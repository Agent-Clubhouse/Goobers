package main

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/diagnostics/history"
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
		includeHostIdentity := setup.Config.Telemetry.EffectiveCollectionProfile().IncludesHostIdentity()
		sink := func(event journal.Event) { records <- serviceHealthDiagnosticRecord(event, includeHostIdentity) }
		emitServiceHealth(ctx, root, identity, setup.InstanceLog, inventory, serviceHealthInterval, nil, nil, sink)
	}()
	go func() {
		defer close(done)
		initialize, cancel := context.WithTimeout(ctx, 5*time.Second)
		exporter, err := buildDiagnosticExporterWithStores(initialize, root, setup, stores)
		cancel()
		if err != nil {
			setup.InstanceLog.AppendBestEffort(journal.Event{Type: journal.EventError, Error: journal.ErrorDetailFor("diagnostics_export_unavailable", err)})
		}
		runHealthExports(ctx, root, setup, exporter, records, fleet)

		if exporter != nil {
			flush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = exporter.Shutdown(flush)
			stats := exporter.Stats()
			setup.InstanceLog.AppendBestEffort(journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{
				"kind": "diagnostics-export-summary", "accepted": stats.Accepted, "delivered": stats.Delivered, "dropped": stats.Dropped, "failures": stats.Failures,
				"azureReplayAccepted": stats.AzureReplay.Accepted, "azureReplayDelivered": stats.AzureReplay.Delivered,
				"azureReplayRetried": stats.AzureReplay.Retried, "azureReplayPrunedAge": stats.AzureReplay.PrunedAge,
				"azureReplayPrunedBytes": stats.AzureReplay.PrunedBytes, "azureReplayMalformed": stats.AzureReplay.Malformed,
			}})
		}
	}()
	return done
}

func buildDiagnosticExporterWithStores(ctx context.Context, root string, setup *schedulerSetup, stores credentials.StoreResolver) (*telemetry.DiagnosticExporter, error) {
	otlp := setup.Config.DiagnosticOTLP()
	azure := setup.Config.Telemetry.AzureMonitor
	azureEnabled := setup.Config.TelemetryEnabled() && azure != nil && azure.Enabled()
	if !otlp.Enabled() && !azureEnabled && !setup.Config.Telemetry.NamedAzureEnabled() {
		return nil, nil
	}
	cfg := telemetry.Config{
		ServiceVersion: version.Get().Version, BuildCommit: version.Get().Commit,
		Scrubber:                journal.Chain(setup.SharedRegistry, journal.NewPatternScrubber()),
		ExporterHealth:          setup.TelemetryExporterHealth,
		AzureMonitorReplayStart: setup.TelemetryReplayStart,
	}
	_, cfg.ResourceAttributes = telemetryInstanceIdentities(root)
	var legacyErr error
	if otlp.Enabled() {
		if err := configureOTLP(ctx, &cfg, otlp, setup.SharedRegistry, stores); err != nil {
			if !setup.Config.Telemetry.NamedAzureEnabled() {
				return nil, err
			}
			legacyErr = err
		}
	}
	if azureEnabled {
		if err := configureAzureMonitor(ctx, &cfg, *azure, setup.Config.Telemetry.EffectiveCollectionProfile(), root, setup.SharedRegistry, stores); err != nil {
			return nil, err
		}
	}
	namedErr := configureNamedTelemetry(ctx, &cfg, setup.Config.Telemetry, root, setup.SharedRegistry, stores, true)
	exporter, err := telemetry.NewDiagnosticExporter(cfg)
	return exporter, errors.Join(legacyErr, namedErr, err)
}

// Whitelist the public operational contract rather than exporting the instance
// journal wholesale. Paths, raw errors, credentials and arbitrary payloads never
// enter this record. Machine/account identity is exported only with explicit
// diagnostic endpoint consent, never through the journal collector.
func serviceHealthDiagnosticRecord(event journal.Event, includeHostIdentity bool) telemetry.DiagnosticRecord {
	attrs := make(map[string]any)
	keys := []string{"schemaVersion", "observedAt", "instanceId", "instanceDisplayName", "identityProblem", "windowCoverage", "daemonStartedAt", "processUptimeSeconds", "observedUncleanRestarts", "observationWindowStart"}
	if includeHostIdentity {
		keys = append(keys, "machineName", "accountName")
	}
	for _, key := range keys {
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

func runHealthExports(ctx context.Context, root string, setup *schedulerSetup, exporter *telemetry.DiagnosticExporter, records <-chan telemetry.DiagnosticRecord, fleet []fleetHealthSample) {
	var scrubber journal.Scrubber = journal.NewPatternScrubber()
	if setup.SharedRegistry != nil {
		scrubber = journal.Chain(setup.SharedRegistry, scrubber)
	}
	store, err := history.Open(ctx, filepath.Join(setup.InstanceLog.Dir(), "diagnostics"), scrubber)
	if store != nil {
		defer func() { _ = store.Close() }()
	}
	historyFailed := false
	recordHistoryFailure := func(err error) {
		if err != nil && !historyFailed {
			historyFailed = true
			setup.InstanceLog.AppendBestEffort(journal.Event{Type: journal.EventError, Error: &journal.ErrorDetail{Code: "diagnostic_history_unavailable", Message: "Local diagnostic history could not retain an observation batch; retained evidence may have gaps."}})
		}
	}
	recordHistoryFailure(err)
	var ticks <-chan time.Time
	if len(fleet) > 0 {
		ticker := time.NewTicker(setup.Config.Telemetry.Diagnostics.HeartbeatPeriod())
		defer ticker.Stop()
		ticks = ticker.C
		recordHistoryFailure(emitFleetHealth(ctx, root, store, exporter, fleet, time.Now().UTC()))
	}
	for {
		select {
		case record, ok := <-records:
			if !ok {
				return
			}
			if exporter != nil {
				addAzureReplayHealth(&record, root)
				exporter.Emit(record)
			}
		case now := <-ticks:
			recordHistoryFailure(emitFleetHealth(ctx, root, store, exporter, fleet, now.UTC()))
		}
	}
}

func addAzureReplayHealth(record *telemetry.DiagnosticRecord, root string) {
	if record == nil {
		return
	}
	if record.Attributes == nil {
		record.Attributes = make(map[string]any)
	}
	pending := telemetry.InspectAzureReplayRoot(filepath.Join(root, "telemetry-export", "azure-monitor"))
	record.Attributes["azureReplayPendingRecords"] = pending.PendingRecords
	record.Attributes["azureReplayPendingBytes"] = pending.PendingBytes
	record.Attributes["azureReplayPendingFiles"] = pending.PendingFiles
	record.Attributes["azureReplayAccountingReady"] = pending.AccountingReady
	record.Attributes["azureReplayAdmissionFailures"] = int64(min(pending.AdmissionFailures, uint64(math.MaxInt64)))
	record.Attributes["azureReplayQueueDropped"] = int64(min(pending.QueueDropped, uint64(math.MaxInt64)))
	record.Attributes["azureReplayExportFailures"] = int64(min(pending.ExportFailures, uint64(math.MaxInt64)))
	record.Attributes["azureReplayOldestPendingSeconds"] = int64(pending.OldestPendingAge.Seconds())
	record.Attributes["azureReplayAccepted"] = int64(min(pending.Accepted, uint64(math.MaxInt64)))
	record.Attributes["azureReplayDelivered"] = int64(min(pending.Delivered, uint64(math.MaxInt64)))
	record.Attributes["azureReplayRetried"] = int64(min(pending.Retried, uint64(math.MaxInt64)))
	record.Attributes["azureReplayPrunedAge"] = int64(min(pending.PrunedAge, uint64(math.MaxInt64)))
	record.Attributes["azureReplayPrunedBytes"] = int64(min(pending.PrunedBytes, uint64(math.MaxInt64)))
	record.Attributes["azureReplayMalformed"] = int64(min(pending.Malformed, uint64(math.MaxInt64)))
	// Delivery evidence (#5940): fixed classes and timestamps, no endpoint.
	record.Attributes["azureReplayActiveFailure"] = pending.ActiveFailure
	if !pending.LastSuccess.IsZero() {
		record.Attributes["azureReplayLastSuccess"] = pending.LastSuccess.UTC().Format(time.RFC3339)
	}
	if !pending.LastFailure.IsZero() {
		record.Attributes["azureReplayLastFailure"] = pending.LastFailure.UTC().Format(time.RFC3339)
		record.Attributes["azureReplayFailureClass"] = pending.FailureClass
	}
}

func emitFleetHealth(ctx context.Context, root string, store *history.Store, exporter *telemetry.DiagnosticExporter, fleet []fleetHealthSample, now time.Time) error {
	var historyErr error
	sampleCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, sample := range fleet {
		records := sample(sampleCtx, now)
		// Persist the canonical fleet schema before adding transport-only export
		// health fields. The bounded local history decoder intentionally rejects
		// fields outside that versioned fleet contract.
		if store != nil {
			if err := store.Append(sampleCtx, records); err != nil {
				historyErr = err
			}
		}
		for _, record := range records {
			if exporter != nil && record.Name == "goobers.fleet.heartbeat" && record.Attributes["gaggleId"] == "" {
				stats := exporter.Stats()
				record.Attributes["diagnosticsDroppedRecords"] = int64(min(stats.Dropped, uint64(math.MaxInt64)))
				addAzureReplayHealth(&record, root)
			}
		}
		for len(records) > 0 {
			count := min(len(records), telemetry.DiagnosticBatchLimit)
			exporter.EmitBatch(records[:count])
			records = records[count:]
		}
	}
	return historyErr
}

// startDaemonHealth starts while startup is still in progress; its deferred
// stop joins observers before the read service and instance journal close.
func startDaemonHealth(ctx context.Context, root string, identity *daemonIdentity, setup *schedulerSetup, inventory recoveryInventorySampler, reader fleetHealthReader, ready func() bool, engines ...*daemonEngineClient) func() {
	healthCtx, cancel := context.WithCancel(ctx)
	var engine *daemonEngineClient
	if len(engines) > 0 {
		engine = engines[0]
	}
	workers := newFleetWorkerHealthObserver(setup, engine)
	health := newFleetHealthSampler(root, identity, setup.Config.Telemetry.Diagnostics, reader, workers, ready)
	done := startServiceHealth(healthCtx, root, identity, setup, inventory, combinedFleetSampler(root, setup, reader, withBacklogHealth(setup, health)))
	return func() { cancel(); <-done }
}
