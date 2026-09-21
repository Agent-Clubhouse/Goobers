package main

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	collectorlogpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

type serviceHealthCollector struct {
	collectorlogpb.UnimplementedLogsServiceServer
	requests chan *collectorlogpb.ExportLogsServiceRequest
	headers  chan metadata.MD
}

func (c *serviceHealthCollector) Export(ctx context.Context, req *collectorlogpb.ExportLogsServiceRequest) (*collectorlogpb.ExportLogsServiceResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	c.headers <- md
	c.requests <- req
	return &collectorlogpb.ExportLogsServiceResponse{}, nil
}
func TestServiceHealthExportProductionWiring(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	collector := &serviceHealthCollector{requests: make(chan *collectorlogpb.ExportLogsServiceRequest, 1), headers: make(chan metadata.MD, 1)}
	server := grpc.NewServer()
	collectorlogpb.RegisterLogsServiceServer(server, collector)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	t.Setenv("DIAGNOSTIC_TEST_AUTH", "private-diagnostic-auth-123456")
	// Journal export points elsewhere; diagnostics must use its own route and credentials.
	cfg := &instance.Config{Telemetry: instance.TelemetryConfig{
		OTLP:        &instance.OTLPConfig{Endpoint: "journal.invalid:4317"},
		Diagnostics: &instance.DiagnosticsConfig{OTLP: &instance.OTLPConfig{Endpoint: listener.Addr().String(), Insecure: true, Headers: map[string]instance.TokenRef{"authorization": {Env: "DIAGNOSTIC_TEST_AUTH"}}}},
	}}
	log := openTestInstanceLog(t)
	setup := &schedulerSetup{Config: cfg, SharedRegistry: journal.NewRegistryScrubber(), InstanceLog: log}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startServiceHealth(ctx, t.TempDir(), &daemonIdentity{StartedAt: time.Now()}, setup, nil)
	select {
	case req := <-collector.requests:
		if !strings.Contains(req.String(), "goobers.service.health") {
			t.Fatalf("wrong export: %s", req)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no startup export through production wiring")
	}
	if md := <-collector.headers; len(md.Get("authorization")) != 1 || md.Get("authorization")[0] != "private-diagnostic-auth-123456" {
		t.Fatal("independent credential missing")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	if len(readServiceHealthEvents(t, log)) != 1 {
		t.Fatal("local health evidence missing")
	}
}
func TestServiceHealthExportWhitelist(t *testing.T) {
	record := serviceHealthDiagnosticRecord(journal.Event{Time: time.Now(), Runner: map[string]any{
		"instanceId": "known", "prompt": "private prompt", "rawConfig": "private config",
		"recoveryInventory": map[string]any{"state": "healthy", "used": 1, "inventoryRoot": "private path", "error": "private raw error"},
	}})
	if len(record.Attributes) != 3 || record.Attributes["instanceId"] != "known" || record.Attributes["recoveryInventory.used"] != 1 {
		t.Fatalf("unexpected public fields: %+v", record.Attributes)
	}
}
func TestServiceHealthDisabledExportDoesNotResolveSecrets(t *testing.T) {
	disabled := false
	cfg := &instance.Config{Telemetry: instance.TelemetryConfig{Diagnostics: &instance.DiagnosticsConfig{OTLP: &instance.OTLPConfig{
		Endpoint: "disabled.invalid:4317", ExportEnabled: &disabled, Headers: map[string]instance.TokenRef{"authorization": {File: "/nonexistent/diagnostic-secret"}},
	}}}}
	exporter, err := buildDiagnosticExporterWithStores(context.Background(), &schedulerSetup{Config: cfg}, nil)
	if err != nil || exporter != nil {
		t.Fatalf("disabled export resolved credentials: %v %v", exporter, err)
	}
}

type blockedDiagnosticStore struct{ entered chan struct{} }

func (s blockedDiagnosticStore) FetchSecret(ctx context.Context, _ string) (string, error) {
	close(s.entered)
	<-ctx.Done()
	return "", ctx.Err()
}
func TestServiceHealthCredentialFailureDoesNotBlockLocalStartup(t *testing.T) {
	cfg := &instance.Config{Telemetry: instance.TelemetryConfig{Diagnostics: &instance.DiagnosticsConfig{OTLP: &instance.OTLPConfig{
		Endpoint: "collector.example:4317", Headers: map[string]instance.TokenRef{"authorization": {Store: "company/collector"}},
	}}}}
	log := openTestInstanceLog(t)
	setup := &schedulerSetup{Config: cfg, SharedRegistry: journal.NewRegistryScrubber(), InstanceLog: log}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	returned := make(chan (<-chan struct{}), 1)
	go func() {
		returned <- startServiceHealthWithStores(ctx, t.TempDir(), nil, setup, nil, blockedDiagnosticStore{entered: entered})
	}()
	var done <-chan struct{}
	select {
	case done = <-returned:
	case <-time.After(time.Second):
		t.Fatal("credential lookup blocked scheduler startup")
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("resolver not entered")
	}
	deadline := time.After(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for len(readServiceHealthEvents(t, log)) == 0 {
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatal("blocked credentials suppressed local health")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("credential lookup prevented shutdown")
	}
}
