package telemetry

import (
	"context"
	"encoding/pem"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	apilog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/sdk/resource"
	collectorlogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

func TestJournalLogsExplicitConfigOverridesEnvironment(t *testing.T) {
	const childKey = "GOOBERS_TEST_JOURNAL_LOGS_ENV"
	if os.Getenv(childKey) == "1" {
		testJournalLogsExplicitConfig(t)
		return
	}
	// Keep ambient-config tests in a subprocess rather than mutating the
	// process environment shared with unrelated telemetry providers.
	server := httptest.NewTLSServer(nil)
	defer server.Close()
	id, err := NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	certFile, err := filepath.Abs(".journal-env-ca-" + id + ".pem")
	if err != nil {
		t.Fatal(err)
	}
	cert := server.TLS.Certificates[0].Certificate[0]
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert}), 0600); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(certFile) }()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestJournalLogsExplicitConfigOverridesEnvironment$", "-test.count=1")
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(name), "OTEL_EXPORTER_OTLP") && name != childKey {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env,
		childKey+"=1",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=http://127.0.0.1:1",
		"OTEL_EXPORTER_OTLP_LOGS_INSECURE=true",
		"OTEL_EXPORTER_OTLP_LOGS_HEADERS=x-journal-env=ambient",
		"OTEL_EXPORTER_OTLP_LOGS_CERTIFICATE="+certFile,
		"OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:2",
		"OTEL_EXPORTER_OTLP_INSECURE=true",
		"OTEL_EXPORTER_OTLP_HEADERS=x-journal-global-env=ambient",
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("isolated ambient-config test: %v\n%s", err, output)
	}
}

func testJournalLogsExplicitConfig(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	receiver := &journalLogsReceiver{
		requests: make(chan *collectorlogspb.ExportLogsServiceRequest, 8),
		headers:  make(chan metadata.MD, 8),
	}
	collectorlogspb.RegisterLogsServiceServer(server, receiver)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()

	for _, insecure := range []bool{true, false} {
		cfg := Config{
			Exporter: ExporterOTLP, OTLPEndpoint: listener.Addr().String(),
			OTLPInsecure: insecure, OTLPHeaders: map[string]string{"x-journal-explicit": "configured"},
		}
		exporter, err := newJournalLogExporter(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		pipeline := newJournalLogPipeline(exporter, resource.Empty(), nil)
		var record apilog.Record
		record.SetBody(attribute.StringValue("{}"))
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		pipeline.logger.Emit(ctx, record)
		cancel()
		ctx, cancel = context.WithTimeout(context.Background(), time.Second)
		_ = pipeline.shutdown(ctx)
		cancel()
		if insecure {
			if pipeline.failures.Load() != 0 {
				t.Fatal("ambient certificate overrode explicit insecure transport")
			}
			select {
			case <-receiver.requests:
			default:
				t.Fatal("explicit endpoint did not receive a record")
			}
			headers := <-receiver.headers
			if headers.Get("x-journal-explicit")[0] != "configured" ||
				len(headers.Get("x-journal-env")) != 0 || len(headers.Get("x-journal-global-env")) != 0 {
				t.Fatalf("ambient headers overrode explicit headers: %v", headers)
			}
		} else {
			if pipeline.failures.Load() != 1 {
				t.Fatal("ambient insecure setting overrode explicit TLS")
			}
			select {
			case <-receiver.requests:
				t.Fatal("TLS client sent logs over plaintext")
			default:
			}
		}
	}
	if _, err := newJournalLogExporter(context.Background(), Config{OTLPEndpoint: "http://%invalid"}); err == nil {
		t.Fatal("malformed explicit endpoint activated SDK environment fallback")
	}
}
