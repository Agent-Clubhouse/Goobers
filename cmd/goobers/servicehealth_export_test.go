package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	collectorlogpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
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
	root := t.TempDir()
	journalID, rootID := strings.Repeat("1", 32), strings.Repeat("2", 32)
	for name, id := range map[string]string{"instance-id": journalID, instance.RootIdentityFileName: rootID} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(id+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	done := startServiceHealth(ctx, root, &daemonIdentity{StartedAt: time.Now()}, setup, nil)
	select {
	case req := <-collector.requests:
		if !strings.Contains(req.String(), "goobers.service.health") {
			t.Fatalf("wrong export: %s", req)
		}
		if len(req.ResourceLogs) != 1 || req.ResourceLogs[0].Resource == nil {
			t.Fatal("diagnostic resource missing")
		}
		attrs := map[string]string{}
		for _, attr := range req.ResourceLogs[0].Resource.Attributes {
			attrs[attr.Key] = attr.Value.GetStringValue()
		}
		if attrs["goobers.instance.id"] != journalID || attrs["goobers.root.id"] != rootID {
			t.Fatalf("diagnostics cannot join journal/root identities: %v", attrs)
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
		"instanceId": "known", "machineName": "workstation-7", "accountName": "alice", "prompt": "private prompt", "rawConfig": "private config",
		"recoveryInventory": map[string]any{"state": "healthy", "used": 1, "inventoryRoot": "private path", "error": "private raw error"},
	}}, false)
	if len(record.Attributes) != 3 || record.Attributes["instanceId"] != "known" || record.Attributes["recoveryInventory.used"] != 1 {
		t.Fatalf("unexpected public fields: %+v", record.Attributes)
	}
	diagnostic := serviceHealthDiagnosticRecord(journal.Event{Time: time.Now(), Runner: map[string]any{
		"instanceId": "known", "machineName": "workstation-7", "accountName": "alice",
	}}, true)
	if diagnostic.Attributes["machineName"] != "workstation-7" || diagnostic.Attributes["accountName"] != "alice" {
		t.Fatalf("diagnostic consent did not include host identity: %+v", diagnostic.Attributes)
	}
}

// Root health records carry persisted delivery evidence (#5940): fixed class
// and timestamps, no endpoint or error text.
func TestAzureReplayHealthCarriesDeliveryEvidence(t *testing.T) {
	root := t.TempDir()
	spool := filepath.Join(root, "telemetry-export", "azure-monitor")
	if err := os.MkdirAll(spool, 0o700); err != nil {
		t.Fatal(err)
	}
	status := `{"schema":"goobers.dev/telemetry/azure-delivery-status/v1","lastSuccess":"2026-10-01T12:00:00Z","lastFailure":"2026-10-01T12:05:00Z","failureClass":"tls"}`
	if err := os.WriteFile(filepath.Join(spool, "status-journal.json"), []byte(status), 0o600); err != nil {
		t.Fatal(err)
	}
	record := telemetry.DiagnosticRecord{}
	addAzureReplayHealth(&record, root)
	if record.Attributes["azureReplayLastSuccess"] != "2026-10-01T12:00:00Z" || record.Attributes["azureReplayLastFailure"] != "2026-10-01T12:05:00Z" ||
		record.Attributes["azureReplayFailureClass"] != "tls" || record.Attributes["azureReplayActiveFailure"] != false {
		t.Fatalf("delivery evidence attributes = %+v", record.Attributes)
	}
}

func TestServiceHealthDisabledExportDoesNotResolveSecrets(t *testing.T) {
	disabled := false
	cfg := &instance.Config{Telemetry: instance.TelemetryConfig{Diagnostics: &instance.DiagnosticsConfig{OTLP: &instance.OTLPConfig{
		Endpoint: "disabled.invalid:4317", ExportEnabled: &disabled, Headers: map[string]instance.TokenRef{"authorization": {File: "/nonexistent/diagnostic-secret"}},
	}}}}
	exporter, err := buildDiagnosticExporterWithStores(context.Background(), t.TempDir(), &schedulerSetup{Config: cfg}, nil)
	if err != nil || exporter != nil {
		t.Fatalf("disabled export resolved credentials: %v %v", exporter, err)
	}
}

func TestServiceHealthUsesUnifiedAzureMonitorDestination(t *testing.T) {
	const connectionString = "InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint=https://example.test/"
	t.Setenv("SERVICE_HEALTH_AZURE_MONITOR", connectionString)
	registry := journal.NewRegistryScrubber()
	cfg := &instance.Config{Telemetry: instance.TelemetryConfig{AzureMonitor: &instance.AzureMonitorConfig{
		ConnectionString: instance.TokenRef{Env: "SERVICE_HEALTH_AZURE_MONITOR"},
	}}}
	exporter, err := buildDiagnosticExporterWithStores(context.Background(), t.TempDir(), &schedulerSetup{Config: cfg, SharedRegistry: registry}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if exporter == nil {
		t.Fatal("unified Azure Monitor destination did not enable diagnostics")
	}
	// Shutdown's bound cannot interrupt the replay index's in-flight SQLite
	// start (#6886). Wait for replay accounting so the bound covers only
	// shutdown work, not a slow index start on a loaded runner.
	for deadline := time.Now().Add(30 * time.Second); !exporter.Stats().AzureReplay.AccountingReady; {
		if time.Now().After(deadline) {
			t.Fatalf("diagnostic replay accounting never became ready: %+v", exporter.Stats().AzureReplay)
		}
		time.Sleep(10 * time.Millisecond)
	}
	shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := exporter.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}
	if got := string(registry.Scrub([]byte(connectionString))); strings.Contains(got, connectionString) {
		t.Fatalf("connection string not registered with diagnostic scrubber: %q", got)
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

func TestFleetExportReportsDroppedRecordsBeforeShutdown(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	collector := &serviceHealthCollector{requests: make(chan *collectorlogpb.ExportLogsServiceRequest, 2), headers: make(chan metadata.MD, 2)}
	server := grpc.NewServer()
	collectorlogpb.RegisterLogsServiceServer(server, collector)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	exporter, err := telemetry.NewDiagnosticExporter(telemetry.Config{OTLPEndpoint: listener.Addr().String(), OTLPInsecure: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := exporter.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	}()
	// Rejected producer input is a real counted loss, without waiting for shutdown.
	if exporter.Emit(telemetry.DiagnosticRecord{}) {
		t.Fatal("invalid input accepted")
	}
	rootRecord := telemetry.DiagnosticRecord{Time: time.Now(), Name: "goobers.fleet.heartbeat", Attributes: map[string]any{"gaggleId": ""}}
	gaggleRecord := telemetry.DiagnosticRecord{Time: rootRecord.Time, Name: rootRecord.Name, Attributes: map[string]any{"gaggleId": "gaggle"}}
	sample := func(context.Context, time.Time) []telemetry.DiagnosticRecord {
		return []telemetry.DiagnosticRecord{rootRecord, gaggleRecord}
	}
	if err := emitFleetHealth(context.Background(), t.TempDir(), nil, exporter, []fleetHealthSample{sample}, rootRecord.Time); err != nil {
		t.Fatal(err)
	}
	if rootRecord.Attributes["diagnosticsDroppedRecords"] != int64(1) {
		t.Fatal(rootRecord.Attributes)
	}
	if _, ok := gaggleRecord.Attributes["diagnosticsDroppedRecords"]; ok {
		t.Fatal("deployment losses attributed to gaggle")
	}
	select {
	case req := <-collector.requests:
		found := false
		for _, resource := range req.ResourceLogs {
			for _, scope := range resource.ScopeLogs {
				for _, record := range scope.LogRecords {
					for _, attr := range record.Attributes {
						if attr.Key == "diagnosticsDroppedRecords" && attr.Value.GetIntValue() == 1 {
							found = true
						}
					}
				}
			}
		}
		if !found {
			t.Fatal("loss count absent from actual OTLP request")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no heartbeat delivered")
	}
	disabled := telemetry.DiagnosticRecord{Time: rootRecord.Time, Name: rootRecord.Name, Attributes: map[string]any{"gaggleId": ""}}
	if err := emitFleetHealth(context.Background(), t.TempDir(), nil, nil, []fleetHealthSample{func(context.Context, time.Time) []telemetry.DiagnosticRecord {
		return []telemetry.DiagnosticRecord{disabled}
	}}, rootRecord.Time); err != nil {
		t.Fatal(err)
	}
	if _, ok := disabled.Attributes["diagnosticsDroppedRecords"]; ok {
		t.Fatal("disabled export falsely reports zero losses")
	}
}
