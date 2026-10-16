package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/telemetry"
)

func TestNamedTelemetryProductionRoutingAndPrivacy(t *testing.T) {
	unsetTestEnv(t, instance.OTLPEndpointEnv)
	unsetTestEnv(t, instance.OTLPInsecureEnv)
	collector, diagnosticCollector := &routingCollector{}, &routingCollector{}
	endpoint, diagnosticEndpoint := startRoutingCollector(t, collector), startRoutingCollector(t, diagnosticCollector)
	var mu sync.Mutex
	var azureBodies []string
	azure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, "gzip", 400)
			return
		}
		defer func() { _ = reader.Close() }()
		body, err := io.ReadAll(reader)
		if err != nil {
			http.Error(w, "body", 400)
			return
		}
		mu.Lock()
		azureBodies = append(azureBodies, string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(azure.Close)
	root := t.TempDir()
	layout := instance.NewLayout(root)
	identity := strings.Repeat("3", 32)
	if err := os.WriteFile(filepath.Join(root, "instance-id"), []byte(identity+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	secretFile := filepath.Join(t.TempDir(), "connection.txt")
	connection := "InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint=" + azure.URL
	if err := os.WriteFile(secretFile, []byte(connection), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NAMED_COLLECTOR_AUTH", "named-collector-secret")
	t.Setenv("NAMED_DIAGNOSTIC_AUTH", "named-diagnostic-secret")
	disabled := false
	cfg := &instance.Config{Telemetry: instance.TelemetryConfig{Exporters: []instance.TelemetryExporterConfig{
		{Name: "collector", Kind: "otlp-grpc", Endpoint: endpoint, Insecure: true, Headers: map[string]instance.TokenRef{"authorization": {Env: "NAMED_COLLECTOR_AUTH"}}},
		{Name: "tenant", Kind: "azuremonitor", Connection: instance.TokenRef{File: secretFile}, Replay: &instance.AzureMonitorReplayConfig{Enabled: &disabled}},
	}, Diagnostics: &instance.DiagnosticsConfig{OTLP: &instance.OTLPConfig{Endpoint: diagnosticEndpoint, Insecure: true, Headers: map[string]instance.TokenRef{"authorization": {Env: "NAMED_DIAGNOSTIC_AUTH"}}}}}}
	if err := instance.WriteConfig(layout.ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	registry, scrubber := journal.DefaultScrubber()
	health := newTelemetryExporterHealth(cfg)
	client, err := buildTelemetryClient(t.Context(), layout, scrubber, registry, cfg.Telemetry, nil, health, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = client.Shutdown(ctx)
	})
	diagnostic, err := buildDiagnosticExporterWithStores(t.Context(), root, &schedulerSetup{Config: cfg, SharedRegistry: registry, TelemetryExporterHealth: health}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = diagnostic.Shutdown(ctx)
	})
	log, _, err := journal.OpenInstanceLog(filepath.Join(root, "scheduler"), journal.WithScrubber(scrubber))
	if err != nil {
		t.Fatal(err)
	}
	log.AppendBestEffort(journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"fixture": "named-collector-secret"}})
	_, span, err := client.StartSchedulerSpan(t.Context(), telemetry.SchedulerAttributes{Gaggle: "fixture", WorkflowID: "fixture", Action: "dispatch"})
	if err != nil {
		t.Fatal(err)
	}
	span.Fail(errors.New("fixture named-collector-secret"))
	diagnostic.Emit(serviceHealthDiagnosticRecord(journal.Event{Time: time.Now(), Runner: map[string]any{"schemaVersion": 1, "instanceId": identity, "machineName": "private-host", "accountName": "private-account"}}, false))
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := diagnostic.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	collector.mu.Lock()
	observations := append([]routingObservation(nil), collector.observations...)
	collector.mu.Unlock()
	signals := map[string]int{}
	for _, o := range observations {
		signals[o.signal]++
		if o.authorization != "named-collector-secret" {
			t.Fatalf("wrong collector credential: %q", o.authorization)
		}
		if strings.Contains(o.payload, "named-collector-secret") || strings.Contains(o.payload, "goobers.service.health") {
			t.Fatalf("collector crossed privacy/diagnostic boundary: %s", o.payload)
		}
	}
	if signals["traces"] != 1 || signals["logs"] != 1 || signals["metrics"] == 0 {
		t.Fatalf("collector signals: %v", signals)
	}
	diagnosticCollector.mu.Lock()
	diagnosticObservations := append([]routingObservation(nil), diagnosticCollector.observations...)
	diagnosticCollector.mu.Unlock()
	if len(diagnosticObservations) != 1 || diagnosticObservations[0].authorization != "named-diagnostic-secret" || !strings.Contains(diagnosticObservations[0].payload, "goobers.service.health") {
		t.Fatalf("independent diagnostics: %+v", diagnosticObservations)
	}
	mu.Lock()
	bodies := strings.Join(azureBodies, "\n")
	mu.Unlock()
	for _, want := range []string{"goobers.service.health", "goobers.journal", "scheduler", identity} {
		if !strings.Contains(bodies, want) {
			t.Fatalf("Azure missing %q", want)
		}
	}
	for _, secret := range []string{"named-collector-secret", connection, "private-host", "private-account"} {
		if strings.Contains(bodies, secret) {
			t.Fatalf("Azure leaked %q", secret)
		}
	}
	spans, err := os.ReadFile(filepath.Join(root, "scheduler", "spans", "spans.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(spans, []byte("\n")) != 1 {
		t.Fatalf("local span emitted more than once: %s", spans)
	}
	status := namedStatus(t, layout, health)
	if len(status.Destinations) != 2 || status.Destinations["tenant"].Journal.Accepted != 1 || status.Destinations["tenant"].Diagnostics.Accepted != 1 {
		t.Fatalf("named status lost counters: %+v", status)
	}
	rendered := telemetryDestinationStatusLines(status)
	if !strings.Contains(rendered, "destination collector") || !strings.Contains(rendered, "destination tenant") {
		t.Fatalf("named renderer: %s", rendered)
	}
	raw, err := json.Marshal(statusJSONOutput{TelemetryExporterHealth: status})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(connection)) || bytes.Contains(raw, []byte(secretFile)) {
		t.Fatalf("status leaked credentials: %s", raw)
	}
}

func namedStatus(t *testing.T, layout instance.Layout, health *telemetry.ExporterHealth) *readservice.TelemetryExporterHealthStatus {
	t.Helper()
	service, err := readservice.NewLocal(readservice.LocalSources{Layout: layout, Definitions: &instance.ConfigSet{Manifest: &apiv1.Manifest{}}, TelemetryExporterHealthStats: health.Snapshot}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	status, err := service.SchedulerStatus(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return status.TelemetryExporterHealth
}

func TestNamedTelemetryMissingCredentialAndBadTLSLeaveHealthyAndLocal(t *testing.T) {
	unsetTestEnv(t, "NAMED_MISSING_SECRET")
	collector := &routingCollector{}
	endpoint := startRoutingCollector(t, collector)
	cfg := instance.TelemetryConfig{Exporters: []instance.TelemetryExporterConfig{
		{Name: "missing", Kind: "azuremonitor", Connection: instance.TokenRef{Env: "NAMED_MISSING_SECRET"}},
		{Name: "tls", Kind: "otlp-grpc", Endpoint: "127.0.0.1:4317", TLS: &instance.OTLPTLSConfig{CAFile: filepath.Join(t.TempDir(), "missing.pem")}},
		{Name: "healthy", Kind: "otlp-grpc", Endpoint: endpoint, Insecure: true},
	}}
	layout := instance.NewLayout(t.TempDir())
	registry, scrubber := journal.DefaultScrubber()
	health := telemetry.NewExporterHealth(true, "custom", "")
	client, err := buildTelemetryClient(t.Context(), layout, scrubber, registry, cfg, nil, health, nil)
	if client == nil || !errors.Is(err, telemetry.ErrOTLPUnavailable) {
		t.Fatalf("client=%v err=%v", client, err)
	}
	_, span, err := client.StartSchedulerSpan(t.Context(), telemetry.SchedulerAttributes{Gaggle: "fixture", WorkflowID: "fixture", Action: "dispatch"})
	if err != nil {
		t.Fatal(err)
	}
	span.End()
	// Healthy trace state is recorded only when the healthy destination's export
	// returns. A bounded Shutdown may abandon that first gRPC export on a loaded
	// runner, so wait for it via ForceFlush before the bounded shutdown.
	flushCtx, cancelFlush := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelFlush()
	if err := client.Flush(flushCtx); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_ = client.Shutdown(ctx)
	if _, err := os.Stat(filepath.Join(layout.Root, "scheduler", "spans", "spans.jsonl")); err != nil {
		t.Fatal("local telemetry lost", err)
	}
	status := namedStatus(t, layout, health)
	if status.Destinations["missing"].UnavailableReason == "" || status.Destinations["tls"].Trace.State != "unhealthy" || status.Destinations["healthy"].Trace.State != "healthy" {
		t.Fatalf("independent health missing: %+v", status)
	}
}

func TestNamedTelemetryStoreReferenceAndNoDiagnosticInheritance(t *testing.T) {
	registry := journal.NewRegistryScrubber()
	var cfg telemetry.Config
	source := instance.TelemetryConfig{Exporters: []instance.TelemetryExporterConfig{{Name: "collector", Kind: "otlp-grpc", Endpoint: "https://collector.example:4317", Headers: map[string]instance.TokenRef{"authorization": {Store: "fixture/token"}}}}}
	if err := configureNamedTelemetry(t.Context(), &cfg, source, t.TempDir(), registry, wiringFakeStoreResolver{"fixture/token": "store-private-value"}, false); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Destinations) != 1 || cfg.Destinations[0].Config.OTLPHeaders["authorization"] != "store-private-value" || string(registry.Scrub([]byte("store-private-value"))) == "store-private-value" {
		t.Fatal("named store reference did not resolve and register")
	}
	diagnostic, err := buildDiagnosticExporterWithStores(t.Context(), t.TempDir(), &schedulerSetup{Config: &instance.Config{Telemetry: source}, SharedRegistry: registry}, nil)
	if err != nil || diagnostic != nil {
		t.Fatalf("journal destination enabled diagnostics: %v %v", diagnostic, err)
	}
}

func TestNamedTelemetryCommandJournalHasOneOwnerAndTwoDestinations(t *testing.T) {
	unsetTestEnv(t, instance.OTLPEndpointEnv)
	unsetTestEnv(t, instance.OTLPInsecureEnv)
	first, second := &routingCollector{}, &routingCollector{}
	cfg := &instance.Config{Telemetry: instance.TelemetryConfig{Exporters: []instance.TelemetryExporterConfig{{Name: "one", Kind: "otlp-grpc", Endpoint: startRoutingCollector(t, first), Insecure: true}, {Name: "two", Kind: "otlp-grpc", Endpoint: startRoutingCollector(t, second), Insecure: true}}}}
	layout := instance.NewLayout(t.TempDir())
	if err := instance.WriteConfig(layout.ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	stopOne := startCommandJournalTelemetry(layout, &stderr)
	stopTwo := startCommandJournalTelemetry(layout, &stderr)
	log, _, err := journal.OpenInstanceLog(filepath.Join(layout.Root, "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	log.AppendBestEffort(journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"fixture": "one-record"}})
	_ = log.Close()
	stopOne()
	stopTwo()
	for i, c := range []*routingCollector{first, second} {
		c.mu.Lock()
		obs := append([]routingObservation(nil), c.observations...)
		c.mu.Unlock()
		if len(obs) != 1 || obs[0].signal != "logs" {
			t.Fatalf("destination %d duplicated or lost command event: %+v; %s", i, obs, stderr.String())
		}
	}
}

func TestNamedTelemetryReplayRootsStayNamedAcrossReorder(t *testing.T) {
	t.Setenv("NAMED_REPLAY_CONNECTION", "InstrumentationKey=fixture;IngestionEndpoint=http://127.0.0.1:1")
	source := instance.TelemetryConfig{Exporters: []instance.TelemetryExporterConfig{{Name: "one", Kind: "azuremonitor", Connection: instance.TokenRef{Env: "NAMED_REPLAY_CONNECTION"}}, {Name: "two", Kind: "azuremonitor", Connection: instance.TokenRef{Env: "NAMED_REPLAY_CONNECTION"}}}}
	root := t.TempDir()
	registry := journal.NewRegistryScrubber()
	var first, second telemetry.Config
	if err := configureNamedTelemetry(t.Context(), &first, source, root, registry, nil, false); err != nil {
		t.Fatal(err)
	}
	source.Exporters[0], source.Exporters[1] = source.Exporters[1], source.Exporters[0]
	if err := configureNamedTelemetry(t.Context(), &second, source, root, registry, nil, false); err != nil {
		t.Fatal(err)
	}
	a, b := first.Destinations[0].Config.AzureMonitorReplayRoot, first.Destinations[1].Config.AzureMonitorReplayRoot
	if a == b || !filepath.IsAbs(a) || a != second.Destinations[1].Config.AzureMonitorReplayRoot || b != second.Destinations[0].Config.AzureMonitorReplayRoot {
		t.Fatalf("unstable spool roots: %q %q", a, b)
	}
	for _, d := range first.Destinations {
		if !d.Config.AzureMonitorTraces || !d.Config.AzureMonitorJournalLogs || d.Config.AzureMonitorReplayMaxBytes != instance.DefaultAzureMonitorReplayMaxBytes {
			t.Fatalf("lost legacy signal/replay defaults for %s", d.Name)
		}
	}
}
