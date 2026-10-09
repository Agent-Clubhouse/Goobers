package telemetry

import (
	"compress/gzip"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/microsoft/ApplicationInsights-Go/appinsights/contracts"
	"go.opentelemetry.io/otel/attribute"
	collectorlogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/goobers/goobers/internal/journal"
)

func TestParseAzureMonitorConnectionString(t *testing.T) {
	tests := []struct {
		name, raw, endpoint, wantError string
	}{
		{
			name: "explicit endpoint",
			raw: "InstrumentationKey=00000000-0000-0000-0000-000000000000;" +
				"IngestionEndpoint=https://westus-0.in.applicationinsights.azure.com/",
			endpoint: "https://westus-0.in.applicationinsights.azure.com/v2.1/track",
		},
		{name: "default endpoint", raw: "InstrumentationKey=key", endpoint: "https://dc.services.visualstudio.com/v2.1/track"},
		{name: "missing key", raw: "IngestionEndpoint=https://example.test", wantError: "InstrumentationKey"},
		{name: "remote plaintext", raw: "InstrumentationKey=key;IngestionEndpoint=http://example.test", wantError: "HTTPS"},
		{name: "userinfo", raw: "InstrumentationKey=key;IngestionEndpoint=https://user@example.test", wantError: "invalid"},
		{name: "duplicate", raw: "InstrumentationKey=one;InstrumentationKey=two", wantError: "repeated"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseAzureMonitorConnectionString(tc.raw)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error = %v, want substring %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.ingestionURL != tc.endpoint {
				t.Fatalf("ingestion URL = %q, want %q", got.ingestionURL, tc.endpoint)
			}
		})
	}
}

func TestAzureMonitorConnectivityProbeIsAcceptedAndIdentityFree(t *testing.T) {
	payloads, server := azureMonitorTestServer(t)
	defer server.Close()

	result, err := TestAzureMonitorConnectivity(
		context.Background(),
		"InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint="+server.URL,
		server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Accepted || result.RecordID == "" || result.Schema != "goobers.dev/telemetry/connectivity/v1" {
		t.Fatalf("unexpected result: %+v", result)
	}
	body := waitAzureMonitorPayload(t, payloads)
	for _, want := range []string{"goobers.telemetry.connectivity", "connectivity_test", result.RecordID} {
		if !strings.Contains(body, want) {
			t.Errorf("payload missing %q: %s", want, body)
		}
	}
	for _, forbidden := range []string{"goobers.instance.id", "goobers.root.id", "goobers.gaggle", "goobers.run.id", "cloud.roleInstance", "ai.device.id", "host.name"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("identity-free probe contains %q: %s", forbidden, body)
		}
	}
}

func TestAzureMonitorConnectivityProbeReportsRejection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer server.Close()

	_, err := TestAzureMonitorConnectivity(
		context.Background(),
		"InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint="+server.URL,
		server.Client(),
	)
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("error = %v, want HTTP 403", err)
	}
}

func TestAzureMonitorHostIdentityRequiresExplicitConsent(t *testing.T) {
	const connectionString = "InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint=https://example.test/"
	hostname, err := os.Hostname()
	if err != nil {
		t.Skip("hostname unavailable")
	}
	standard, err := newAzureMonitorClient(connectionString, nil, false, azureReplayConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if standard.defaultTags[contracts.DeviceId] != "" || standard.defaultTags[contracts.CloudRoleInstance] != "" {
		t.Fatal("standard profile exposed hostname")
	}
	diagnostic, err := newAzureMonitorClient(connectionString, nil, true, azureReplayConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if diagnostic.defaultTags[contracts.DeviceId] != hostname || diagnostic.defaultTags[contracts.CloudRoleInstance] != hostname {
		t.Fatal("diagnostic profile omitted consented hostname")
	}
}

func TestAzureMonitorSignalsAreSelectedIndependently(t *testing.T) {
	const connectionString = "InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint=https://example.test/"
	exporters, err := spanExporters(context.Background(), Config{AzureMonitorConnectionString: connectionString})
	if err != nil {
		t.Fatal(err)
	}
	if len(exporters) != 0 {
		t.Fatalf("disabled Azure traces created %d exporters", len(exporters))
	}
	client, err := New(context.Background(), Config{
		AzureMonitorConnectionString: connectionString,
		JournalLogsOnly:              true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.JournalLogsEnabled() {
		t.Fatal("disabled Azure journals created a journal pipeline")
	}
	if err := client.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAzureMonitorExporterSendsCorrelatedScrubbedSpan(t *testing.T) {
	payloads := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2.1/track" {
			t.Errorf("path = %q", r.URL.Path)
		}
		reader, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("gzip reader: %v", err)
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		payloads <- string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"itemsReceived":1,"itemsAccepted":1,"errors":[]}`)
	}))
	defer server.Close()

	const secret = "tenant-secret-value"
	registry := journal.NewRegistryScrubber()
	registry.Register([]byte(secret))
	scrubber := journal.Chain(registry, journal.NewPatternScrubber())
	client, err := New(context.Background(), Config{
		ServiceName: "goobers", ServiceVersion: "v0.5.0-test", BuildCommit: "abc123",
		AzureMonitorConnectionString: "InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint=" + server.URL,
		AzureMonitorTraces:           true,
		AzureMonitorHTTPClient:       server.Client(), Scrubber: scrubber,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, span, err := client.StartRun(context.Background(), RunAttributes{
		Gaggle: "tenant-gaggle", WorkflowID: "poll", RunID: "0af7651916cd43dd8448eb211c80319c",
	})
	if err != nil {
		t.Fatal(err)
	}
	span.Succeed("completed " + secret)
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}

	select {
	case body := <-payloads:
		for _, want := range []string{"tenant-gaggle", "poll", "0af7651916cd43dd8448eb211c80319c", "v0.5.0-test", "abc123"} {
			if !strings.Contains(body, want) {
				t.Errorf("payload missing %q: %s", want, body)
			}
		}
		if strings.Contains(body, secret) {
			t.Fatalf("payload contains registered secret: %s", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Application Insights request")
	}
}

func TestAzureMonitorExporterSendsCommittedJournalLog(t *testing.T) {
	payloads, server := azureMonitorTestServer(t)
	defer server.Close()

	const secret = "journal-secret-value"
	registry := journal.NewRegistryScrubber()
	registry.Register([]byte(secret))
	scrubber := journal.Chain(registry, journal.NewPatternScrubber())
	client, err := New(context.Background(), Config{
		ServiceName: "goobers", ServiceVersion: "v0.5.0-test", BuildCommit: "journal-build",
		AzureMonitorConnectionString: "InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint=" + server.URL,
		AzureMonitorJournalLogs:      true,
		AzureMonitorHTTPClient:       server.Client(), JournalLogs: true, JournalLogsOnly: true,
		Scrubber:           scrubber,
		ResourceAttributes: []attribute.KeyValue{attribute.String("goobers.instance.id", "durable-instance")},
	})
	if err != nil {
		t.Fatal(err)
	}
	client.Commit(journal.CommittedEvent{
		Kind: "run", JournalID: "0af7651916cd43dd8448eb211c80319c", RunID: "0af7651916cd43dd8448eb211c80319c",
		InstanceID: "durable-instance", Gaggle: "tenant-gaggle", Seq: 7, Time: time.Now(),
		// CommittedEvent.Body is the already-scrubbed authoritative journal
		// payload; direct Commit in this test mirrors that boundary contract.
		Body: scrubber.Scrub([]byte(`{"type":"stage.failed","error":"` + secret + `"}`)),
	})
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}

	body := waitAzureMonitorPayload(t, payloads)
	for _, want := range []string{"stage.failed", "[REDACTED]", "goobers.journal", "durable-instance", "tenant-gaggle", "0af7651916cd43dd8448eb211c80319c", `"goobers.journal.seq":"7"`} {
		if !strings.Contains(body, want) {
			t.Errorf("payload missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, secret) {
		t.Fatalf("payload contains registered secret: %s", body)
	}
}

func TestAzureMonitorJournalReplaySurvivesRestartWithStableScrubbedIdentity(t *testing.T) {
	var available atomic.Bool
	var failedMu sync.Mutex
	var failedPayload string
	rejected := make(chan struct{})
	var rejectOnce sync.Once
	delivered := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(reader)
		_ = reader.Close()
		if !available.Load() {
			failedMu.Lock()
			if failedPayload == "" {
				failedPayload = string(body)
			}
			failedMu.Unlock()
			w.WriteHeader(http.StatusServiceUnavailable)
			rejectOnce.Do(func() { close(rejected) })
			return
		}
		delivered <- string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	const secret = "spool-secret-must-never-land"
	registry := journal.NewRegistryScrubber()
	registry.Register([]byte(secret))
	scrubber := journal.Chain(registry, journal.NewPatternScrubber())
	replayRoot := t.TempDir()
	cfg := Config{
		AzureMonitorConnectionString: "InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint=" + server.URL,
		AzureMonitorHTTPClient:       server.Client(), AzureMonitorJournalLogs: true, JournalLogs: true, JournalLogsOnly: true,
		AzureMonitorReplayRoot: replayRoot, AzureMonitorReplayMaxAge: 72 * time.Hour, AzureMonitorReplayMaxBytes: 1 << 20,
		Scrubber: scrubber,
	}
	first, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	first.Commit(journal.CommittedEvent{
		Kind: "run", JournalID: "0af7651916cd43dd8448eb211c80319c", RunID: "0af7651916cd43dd8448eb211c80319c",
		Seq: 1, Time: time.Now(), Body: scrubber.Scrub([]byte(`{"type":"auth.failed","detail":"` + secret + `"}`)),
	})
	// Shutdown is deliberately bounded and abandons work still queued when its
	// deadline expires. Wait for durable admission and the replay worker's
	// failed attempt first, so a slow index start cannot turn this into a
	// shutdown-cancelled admission instead of an unavailable destination.
	select {
	case <-rejected:
	case <-time.After(30 * time.Second):
		t.Fatalf("replay worker never attempted the unavailable destination: %+v", first.JournalExportStats())
	}
	for deadline := time.Now().Add(10 * time.Second); first.JournalExportStats().AzureReplay.Retried == 0; {
		if time.Now().After(deadline) {
			t.Fatalf("rejected delivery was not recorded as a retry: %+v", first.JournalExportStats())
		}
		time.Sleep(10 * time.Millisecond)
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	if err := first.Shutdown(shutdown); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	if stats := first.JournalExportStats(); stats.AzureReplay.Retried == 0 {
		t.Fatalf("unavailable destination was not reflected in export stats: %+v", stats)
	}
	spooled, err := filepath.Glob(filepath.Join(replayRoot, "journal", "*.ndjson"))
	if err != nil || len(spooled) != 1 {
		t.Fatalf("spooled batches = %v, %v", spooled, err)
	}
	onDisk, err := os.ReadFile(spooled[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(onDisk), secret) || !strings.Contains(string(onDisk), "[REDACTED]") {
		t.Fatalf("spool did not retain only scrubbed content: %s", onDisk)
	}

	available.Store(true)
	second, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	var replayed string
	select {
	case replayed = <-delivered:
	case <-time.After(5 * time.Second):
		t.Fatal("restart did not replay the pending journal batch")
	}
	shutdown, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := second.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}
	failedMu.Lock()
	failed := failedPayload
	failedMu.Unlock()
	failedID, replayedID := azureRecordID(failed), azureRecordID(replayed)
	if failedID == "" || replayedID != failedID {
		t.Fatalf("record IDs changed across replay: failed=%q replayed=%q", failedID, replayedID)
	}
	spooled, _ = filepath.Glob(filepath.Join(replayRoot, "journal", "*.ndjson"))
	if len(spooled) != 0 {
		t.Fatalf("acknowledged replay remained pending: %v", spooled)
	}
}

func azureRecordID(payload string) string {
	const marker = `"goobers.telemetry.record_id":"`
	start := strings.Index(payload, marker)
	if start < 0 {
		return ""
	}
	rest := payload[start+len(marker):]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return ""
	}
	return rest[:end]
}

func TestAzureMonitorExporterSendsWhitelistedDiagnosticLog(t *testing.T) {
	payloads, server := azureMonitorTestServer(t)
	defer server.Close()

	const secret = "diagnostic-secret-value"
	registry := journal.NewRegistryScrubber()
	registry.Register([]byte(secret))
	exporter, err := NewDiagnosticExporter(Config{
		ServiceVersion: "v0.5.0-test", BuildCommit: "diagnostic-build",
		AzureMonitorConnectionString: "InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint=" + server.URL,
		AzureMonitorHTTPClient:       server.Client(), Scrubber: journal.Chain(registry, journal.NewPatternScrubber()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !exporter.Emit(DiagnosticRecord{Time: time.Now(), Name: "goobers.service.health", Attributes: map[string]any{
		"instanceId": "durable-instance", "machineName": "windows-worker-07", "reasonCode": "auth_rejected", "detail": secret,
	}}) {
		t.Fatal("diagnostic record was not accepted")
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exporter.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}
	if stats := exporter.Stats(); stats.Accepted != 1 || stats.Delivered != 1 || stats.Dropped != 0 {
		t.Fatalf("diagnostic stats = %+v", stats)
	}

	body := waitAzureMonitorPayload(t, payloads)
	for _, want := range []string{"goobers.service.health", "goobers.diagnostics", "durable-instance", "windows-worker-07", "auth_rejected", "[REDACTED]"} {
		if !strings.Contains(body, want) {
			t.Errorf("payload missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, secret) {
		t.Fatalf("payload contains registered secret: %s", body)
	}
}

func TestJournalLogsFanOutToOTLPAndAzureMonitor(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := grpc.NewServer()
	receiver := &journalLogsReceiver{
		requests: make(chan *collectorlogspb.ExportLogsServiceRequest, 1),
		headers:  make(chan metadata.MD, 1),
	}
	collectorlogspb.RegisterLogsServiceServer(grpcServer, receiver)
	go func() { _ = grpcServer.Serve(listener) }()
	defer grpcServer.Stop()

	payloads, azureServer := azureMonitorTestServer(t)
	defer azureServer.Close()
	client, err := New(context.Background(), Config{
		Exporter: ExporterOTLP, OTLPEndpoint: listener.Addr().String(), OTLPInsecure: true,
		AzureMonitorConnectionString: "InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint=" + azureServer.URL,
		AzureMonitorJournalLogs:      true,
		AzureMonitorHTTPClient:       azureServer.Client(), JournalLogs: true, JournalLogsOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	client.Commit(journal.CommittedEvent{Kind: "scheduler", JournalID: "fanout-journal", Seq: 1, Time: time.Now(), Body: []byte(`{"type":"scheduler.tick"}`)})
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}

	select {
	case request := <-receiver.requests:
		if !strings.Contains(request.String(), "fanout-journal") {
			t.Fatalf("OTLP export missing journal identity: %s", request)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OTLP destination received no journal record")
	}
	if body := waitAzureMonitorPayload(t, payloads); !strings.Contains(body, "fanout-journal") {
		t.Fatalf("Azure Monitor export missing journal identity: %s", body)
	}
}

func TestDiagnosticLogsFanOutToOTLPAndAzureMonitor(t *testing.T) {
	collector := &diagnosticTestCollector{
		requests: make(chan *collectorlogspb.ExportLogsServiceRequest, 1),
		headers:  make(chan metadata.MD, 1),
	}
	endpoint := startDiagnosticTestCollector(t, collector)
	payloads, azureServer := azureMonitorTestServer(t)
	defer azureServer.Close()
	exporter, err := NewDiagnosticExporter(Config{
		OTLPEndpoint: endpoint, OTLPInsecure: true,
		AzureMonitorConnectionString: "InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint=" + azureServer.URL,
		AzureMonitorHTTPClient:       azureServer.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !exporter.Emit(DiagnosticRecord{Time: time.Now(), Name: "goobers.fleet.heartbeat", Attributes: map[string]any{"instanceId": "fanout-instance"}}) {
		t.Fatal("diagnostic fanout record was not accepted")
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exporter.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-collector.requests:
		if !strings.Contains(request.String(), "fanout-instance") {
			t.Fatalf("OTLP diagnostic export missing identity: %s", request)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OTLP destination received no diagnostic record")
	}
	if body := waitAzureMonitorPayload(t, payloads); !strings.Contains(body, "fanout-instance") {
		t.Fatalf("Azure Monitor diagnostic export missing identity: %s", body)
	}
}

func TestAzureMonitorRejectionIsVisibleInLogExportStats(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	connectionString := "InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint=" + server.URL

	diagnostic, err := NewDiagnosticExporter(Config{AzureMonitorConnectionString: connectionString, AzureMonitorHTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if !diagnostic.Emit(DiagnosticRecord{Time: time.Now(), Name: "goobers.service.health"}) {
		t.Fatal("diagnostic record was not accepted")
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := diagnostic.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}
	if stats := diagnostic.Stats(); stats.Accepted != 1 || stats.Delivered != 0 || stats.Dropped != 1 || stats.Failures != 1 {
		t.Fatalf("diagnostic rejection stats = %+v", stats)
	}

	journalClient, err := New(context.Background(), Config{
		AzureMonitorConnectionString: connectionString, AzureMonitorHTTPClient: server.Client(), AzureMonitorJournalLogs: true,
		JournalLogs: true, JournalLogsOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	journalClient.Commit(journal.CommittedEvent{Kind: "scheduler", JournalID: "rejected-journal", Time: time.Now(), Body: []byte(`{"type":"test"}`)})
	if err := journalClient.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}
	if stats := journalClient.JournalExportStats(); stats.Accepted != 1 || stats.ExportFailures != 1 {
		t.Fatalf("journal rejection stats = %+v", stats)
	}
}

func azureMonitorTestServer(t *testing.T) (chan string, *httptest.Server) {
	t.Helper()
	payloads := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("gzip reader: %v", err)
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		payloads <- string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"itemsReceived":1,"itemsAccepted":1,"errors":[]}`)
	}))
	return payloads, server
}

func waitAzureMonitorPayload(t *testing.T, payloads <-chan string) string {
	t.Helper()
	select {
	case body := <-payloads:
		return body
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Application Insights request")
		return ""
	}
}
