package main

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	collectorlog "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectormetric "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

type routingObservation struct{ signal, authorization, payload string }
type routingCollector struct {
	mu                           sync.Mutex
	observations                 []routingObservation
	failJournal, failDiagnostics bool
}

func (c *routingCollector) record(ctx context.Context, signal, payload string) error {
	md, _ := metadata.FromIncomingContext(ctx)
	c.mu.Lock()
	c.observations = append(c.observations, routingObservation{signal: signal, authorization: strings.Join(md.Get("authorization"), ","), payload: payload})
	c.mu.Unlock()
	if signal == "logs" && c.failDiagnostics || signal != "logs" && c.failJournal {
		return status.Error(codes.PermissionDenied, "fixture rejects this destination")
	}
	return nil
}

type routingTraces struct {
	collectortrace.UnimplementedTraceServiceServer
	collector *routingCollector
}

func (s *routingTraces) Export(ctx context.Context, req *collectortrace.ExportTraceServiceRequest) (*collectortrace.ExportTraceServiceResponse, error) {
	return &collectortrace.ExportTraceServiceResponse{}, s.collector.record(ctx, "traces", req.String())
}

type routingLogs struct {
	collectorlog.UnimplementedLogsServiceServer
	collector *routingCollector
}

func (s *routingLogs) Export(ctx context.Context, req *collectorlog.ExportLogsServiceRequest) (*collectorlog.ExportLogsServiceResponse, error) {
	return &collectorlog.ExportLogsServiceResponse{}, s.collector.record(ctx, "logs", req.String())
}

type routingMetrics struct {
	collectormetric.UnimplementedMetricsServiceServer
	collector *routingCollector
}

func (s *routingMetrics) Export(ctx context.Context, req *collectormetric.ExportMetricsServiceRequest) (*collectormetric.ExportMetricsServiceResponse, error) {
	return &collectormetric.ExportMetricsServiceResponse{}, s.collector.record(ctx, "metrics", req.String())
}
func startRoutingCollector(t *testing.T, collector *routingCollector) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	collectortrace.RegisterTraceServiceServer(server, &routingTraces{collector: collector})
	collectorlog.RegisterLogsServiceServer(server, &routingLogs{collector: collector})
	collectormetric.RegisterMetricsServiceServer(server, &routingMetrics{collector: collector})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })
	return listener.Addr().String()
}

type routingCase struct {
	name                                                     string
	journal, diagnostics, same, failJournal, failDiagnostics bool
}

// These are production configuration/build seams and real OTLP gRPC services:
// endpoint selection, credentials, signal envelopes, flush, and a failed
// destination are exercised together. Single-exporter TLS/queue tests live in
// telemetry; this matrix covers isolation between the two live transports.
func TestOTLPJournalDiagnosticRoutingMatrix(t *testing.T) {
	for _, tc := range []routingCase{
		{name: "journal only", journal: true},
		{name: "diagnostics only with run telemetry disabled", diagnostics: true},
		{name: "separate collectors", journal: true, diagnostics: true},
		{name: "same collector", journal: true, diagnostics: true, same: true},
		{name: "both exports explicitly disabled"},
		{name: "journal rejected diagnostics delivered", journal: true, diagnostics: true, failJournal: true},
		{name: "diagnostics rejected journal delivered", journal: true, diagnostics: true, failDiagnostics: true},
	} {
		t.Run(tc.name, func(t *testing.T) { runRoutingCase(t, tc) })
	}
}

func runRoutingCase(t *testing.T, tc routingCase) {
	t.Helper()
	first := &routingCollector{failJournal: tc.failJournal, failDiagnostics: tc.same && tc.failDiagnostics}
	second := &routingCollector{failDiagnostics: tc.failDiagnostics}
	journalEndpoint := startRoutingCollector(t, first)
	diagnosticEndpoint := startRoutingCollector(t, second)
	if tc.same {
		diagnosticEndpoint = journalEndpoint
	}
	t.Setenv("ROUTING_JOURNAL_TOKEN", "private-journal-route-token")
	t.Setenv("ROUTING_DIAGNOSTIC_TOKEN", "private-diagnostic-route-token")
	cfg := routingConfiguration(tc, journalEndpoint, diagnosticEndpoint)
	var dnsCalls atomic.Int32
	if !tc.journal && !tc.diagnostics {
		installRoutingDNSGuard(t, &dnsCalls)
	}
	resolved, err := cfg.ResolveOTLPConfig(func(name string) (string, bool) {
		if !tc.journal && !tc.diagnostics {
			return os.LookupEnv(name)
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := journal.NewRegistryScrubber()
	scrubber := journal.Chain(registry, journal.NewPatternScrubber())
	var client *telemetry.Client
	if cfg.TelemetryEnabled() {
		client, err = buildTelemetryClient(context.Background(), instance.NewLayout(t.TempDir()), scrubber, registry, resolved, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = client.Shutdown(ctx)
		})
	}
	diagnostic, err := buildDiagnosticExporterWithStores(context.Background(), &schedulerSetup{Config: cfg, SharedRegistry: registry}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = diagnostic.Shutdown(ctx)
	})
	if (diagnostic != nil) != tc.diagnostics {
		t.Fatalf("diagnostic opt-in mismatch: %v", diagnostic != nil)
	}
	if client != nil {
		_, span, err := client.StartRun(context.Background(), telemetry.RunAttributes{Gaggle: "routing-gaggle", WorkflowID: "routing-workflow", RunID: "0af7651916cd43dd8448eb211c80319c"})
		if err != nil {
			t.Fatal(err)
		}
		span.End()
	}
	if diagnostic != nil && !diagnostic.Emit(serviceHealthDiagnosticRecord(journal.Event{Time: time.Now(), Runner: map[string]any{"schemaVersion": 1, "instanceId": "routing-diagnostic-instance"}})) {
		t.Fatal("diagnostic observation rejected")
	}
	journalDone, diagnosticDone := make(chan error, 1), make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if client != nil {
			journalDone <- client.Shutdown(ctx)
		} else {
			journalDone <- nil
		}
	}()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		diagnosticDone <- diagnostic.Shutdown(ctx)
	}()
	if err := awaitRoutingShutdown(t, journalDone); err != nil && !tc.failJournal {
		t.Fatal(err)
	}
	if err := awaitRoutingShutdown(t, diagnosticDone); err != nil {
		t.Fatal(err)
	}
	if diagnostic != nil {
		stats := diagnostic.Stats()
		if tc.failDiagnostics {
			if stats.Failures == 0 || stats.Delivered != 0 {
				t.Fatalf("diagnostic failure unobserved: %+v", stats)
			}
		} else if stats.Delivered != 1 {
			t.Fatalf("healthy diagnostics blocked: %+v", stats)
		}
	}
	assertRoutingCollector(t, first, tc.journal, tc.diagnostics && tc.same)
	assertRoutingCollector(t, second, false, tc.diagnostics && !tc.same)
	if dnsCalls.Load() != 0 {
		t.Fatalf("disabled exporter attempted DNS %d times", dnsCalls.Load())
	}
}

func routingConfiguration(tc routingCase, journalEndpoint, diagnosticEndpoint string) *instance.Config {
	disabled := false
	cfg := &instance.Config{Telemetry: instance.TelemetryConfig{}}
	if tc.journal {
		cfg.Telemetry.OTLP = &instance.OTLPConfig{Endpoint: journalEndpoint, Insecure: true, Headers: map[string]instance.TokenRef{"authorization": {Env: "ROUTING_JOURNAL_TOKEN"}}}
	}
	if tc.diagnostics {
		cfg.Telemetry.Diagnostics = &instance.DiagnosticsConfig{OTLP: &instance.OTLPConfig{Endpoint: diagnosticEndpoint, Insecure: true, Headers: map[string]instance.TokenRef{"authorization": {Env: "ROUTING_DIAGNOSTIC_TOKEN"}}}}
	}
	if tc.diagnostics && !tc.journal {
		cfg.Telemetry.Enabled = &disabled
	}
	if !tc.journal && !tc.diagnostics {
		cfg.Telemetry.OTLP = &instance.OTLPConfig{Endpoint: "disabled-journal.invalid:4317", ExportEnabled: &disabled, Headers: map[string]instance.TokenRef{"authorization": {File: "/nonexistent/disabled-journal-secret"}}}
		cfg.Telemetry.Diagnostics = &instance.DiagnosticsConfig{OTLP: &instance.OTLPConfig{Endpoint: "disabled-diagnostic.invalid:4317", ExportEnabled: &disabled, Headers: map[string]instance.TokenRef{"authorization": {File: "/nonexistent/disabled-diagnostic-secret"}}}}
	}
	return cfg
}
func installRoutingDNSGuard(t *testing.T, calls *atomic.Int32) {
	t.Helper()
	t.Setenv(instance.OTLPEndpointEnv, "ambient-journal.invalid:4317")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://ambient-generic.invalid:4317")
	previous := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		calls.Add(1)
		return nil, errors.New("disabled export DNS guard")
	}}
	t.Cleanup(func() { net.DefaultResolver = previous })
}
func awaitRoutingShutdown(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(8 * time.Second):
		t.Fatal("independent exporter did not stop within bound")
		return nil
	}
}
func assertRoutingCollector(t *testing.T, c *routingCollector, wantJournal, wantDiagnostics bool) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	traces, logs := 0, 0
	for _, observation := range c.observations {
		if strings.Contains(observation.payload, "private-journal-route-token") || strings.Contains(observation.payload, "private-diagnostic-route-token") {
			t.Fatal("collector credential leaked into record")
		}
		switch observation.signal {
		case "traces":
			traces++
			if !wantJournal || observation.authorization != "private-journal-route-token" || !strings.Contains(observation.payload, "routing-gaggle") {
				t.Fatal("journal misrouted", observation.signal)
			}
		case "metrics":
			if !wantJournal || observation.authorization != "private-journal-route-token" {
				t.Fatal("journal metrics misrouted")
			}
		case "logs":
			logs++
			if !wantDiagnostics || observation.authorization != "private-diagnostic-route-token" || !strings.Contains(observation.payload, "goobers.telemetry.stream") || !strings.Contains(observation.payload, "diagnostics") || !strings.Contains(observation.payload, "routing-diagnostic-instance") {
				t.Fatal("diagnostics misrouted")
			}
		}
	}
	if (traces > 0) != wantJournal || (logs > 0) != wantDiagnostics {
		t.Fatalf("received traces=%d logs=%d, expected journal=%v diagnostics=%v", traces, logs, wantJournal, wantDiagnostics)
	}
}
