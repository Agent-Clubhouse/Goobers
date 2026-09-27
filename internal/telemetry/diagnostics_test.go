package telemetry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"math"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	collectorlogpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"

	"github.com/goobers/goobers/internal/journal"
)

type diagnosticTestCollector struct {
	collectorlogpb.UnimplementedLogsServiceServer
	requests chan *collectorlogpb.ExportLogsServiceRequest
	headers  chan metadata.MD
	block    bool
	reject   bool
}

func (c *diagnosticTestCollector) Export(ctx context.Context, r *collectorlogpb.ExportLogsServiceRequest) (*collectorlogpb.ExportLogsServiceResponse, error) {
	c.requests <- r
	md, _ := metadata.FromIncomingContext(ctx)
	c.headers <- md
	if c.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	response := &collectorlogpb.ExportLogsServiceResponse{}
	if c.reject {
		response.PartialSuccess = &collectorlogpb.ExportLogsPartialSuccess{RejectedLogRecords: 1}
	}
	return response, nil
}
func startDiagnosticTestCollector(t *testing.T, c *diagnosticTestCollector) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer()
	collectorlogpb.RegisterLogsServiceServer(s, c)
	go func() { _ = s.Serve(l) }()
	t.Cleanup(s.Stop)
	return l.Addr().String()
}
func diagnosticTestRecord() DiagnosticRecord {
	return DiagnosticRecord{Time: time.Now(), Name: "goobers.service.health", Attributes: map[string]any{"instanceId": "test-instance"}}
}
func TestDiagnosticExporterDelivery(t *testing.T) {
	c := &diagnosticTestCollector{requests: make(chan *collectorlogpb.ExportLogsServiceRequest, 1), headers: make(chan metadata.MD, 1)}
	addr := startDiagnosticTestCollector(t, c)
	registry := journal.NewRegistryScrubber()
	registry.Register([]byte("private-value-123456"))
	d, err := NewDiagnosticExporter(Config{OTLPEndpoint: addr, OTLPInsecure: true, OTLPHeaders: map[string]string{"authorization": "Bearer collector-secret"}, ServiceVersion: "v0.5.0", Scrubber: registry})
	if err != nil {
		t.Fatal(err)
	}
	r := diagnosticTestRecord()
	r.Attributes["identityProblem"] = "private-value-123456"
	if !d.Emit(r) {
		t.Fatal("emit rejected")
	}
	r.Attributes["instanceId"] = "mutated"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	got := <-c.requests
	if strings.Contains(got.String(), "private-value-123456") || strings.Contains(got.String(), "mutated") {
		t.Fatalf("unsafe snapshot: %s", got)
	}
	if !strings.Contains(got.String(), "test-instance") || !strings.Contains(got.String(), "v0.5.0") {
		t.Fatalf("missing identity/build: %s", got)
	}
	if h := <-c.headers; h.Get("authorization")[0] != "Bearer collector-secret" {
		t.Fatal("missing authentication")
	}
	if s := d.Stats(); s.Accepted != 1 || s.Delivered != 1 || s.Dropped != 0 {
		t.Fatalf("stats: %+v", s)
	}
	if d.Emit(r) {
		t.Fatal("accepted after shutdown")
	}
}
func TestDiagnosticExporterBoundsAndShutdown(t *testing.T) {
	c := &diagnosticTestCollector{requests: make(chan *collectorlogpb.ExportLogsServiceRequest, 1), headers: make(chan metadata.MD, 1), block: true}
	d, err := NewDiagnosticExporter(Config{OTLPEndpoint: startDiagnosticTestCollector(t, c), OTLPInsecure: true})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Emit(diagnosticTestRecord()) {
		t.Fatal("first emit rejected")
	}
	select {
	case <-c.requests:
	case <-time.After(5 * time.Second):
		t.Fatal("no export")
	}
	for i := 0; i < DiagnosticQueueLimit; i++ {
		if !d.Emit(diagnosticTestRecord()) {
			t.Fatalf("queue rejected record %d", i)
		}
	}
	if d.Emit(diagnosticTestRecord()) {
		t.Fatal("queue overflow accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if d.Shutdown(ctx) == nil {
		t.Fatal("expected shutdown deadline")
	}
	select {
	case <-d.done:
	default:
		t.Fatal("shutdown returned before final accounting")
	}
	s := d.Stats()
	if s.Accepted != DiagnosticQueueLimit+1 || s.Dropped != DiagnosticQueueLimit+2 || s.Delivered != 0 {
		t.Fatalf("loss accounting: %+v", s)
	}
}
func TestDiagnosticExporterRejectsUnsafePayloads(t *testing.T) {
	d := &DiagnosticExporter{}
	for _, v := range []any{strings.Repeat("x", DiagnosticRecordLimit+1), map[string]any{"prompt": "private"}, math.NaN(), math.Inf(1)} {
		r := diagnosticTestRecord()
		r.Attributes["value"] = v
		if d.Emit(r) {
			t.Fatalf("accepted %T", v)
		}
	}
	if d.Stats().Dropped != 4 {
		t.Fatal(d.Stats())
	}
}
func TestDiagnosticExporterDisabledIgnoresEnvironment(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://ambient.invalid:4317")
	t.Setenv("GOOBERS_OTLP_ENDPOINT", "http://ambient.invalid:4317")
	d, err := NewDiagnosticExporter(Config{})
	if err != nil || d != nil {
		t.Fatalf("disabled exporter: %v %v", d, err)
	}
}
func TestDiagnosticExporterPartialRejection(t *testing.T) {
	c := &diagnosticTestCollector{requests: make(chan *collectorlogpb.ExportLogsServiceRequest, 1), headers: make(chan metadata.MD, 1), reject: true}
	d, err := NewDiagnosticExporter(Config{OTLPEndpoint: startDiagnosticTestCollector(t, c), OTLPInsecure: true})
	if err != nil {
		t.Fatal(err)
	}
	d.Emit(diagnosticTestRecord())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if s := d.Stats(); s.Delivered != 0 || s.Dropped != 1 || s.Failures != 1 {
		t.Fatalf("rejection not counted: %+v", s)
	}
}

func TestDiagnosticExporterMutualTLS(t *testing.T) {
	serverCert := generateOTLPTestCertificate(t, []string{"diagnostics.test"}, nil)
	clientCert := generateOTLPTestCertificate(t, nil, nil)
	pair, err := tls.LoadX509KeyPair(serverCert.certFile, serverCert.keyFile)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := os.ReadFile(clientCert.certFile)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool})))
	c := &diagnosticTestCollector{requests: make(chan *collectorlogpb.ExportLogsServiceRequest, 1), headers: make(chan metadata.MD, 1)}
	collectorlogpb.RegisterLogsServiceServer(server, c)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	for _, authenticated := range []bool{false, true} {
		cfg := Config{OTLPEndpoint: listener.Addr().String(), OTLPCAFile: serverCert.certFile, OTLPServerName: "diagnostics.test"}
		if authenticated {
			cfg.OTLPCertFile = clientCert.certFile
			cfg.OTLPKeyFile = clientCert.keyFile
		}
		d, err := NewDiagnosticExporter(cfg)
		if err != nil {
			t.Fatal(err)
		}
		d.Emit(diagnosticTestRecord())
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = d.Shutdown(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		s := d.Stats()
		if authenticated && s.Delivered != 1 {
			t.Fatalf("mTLS did not deliver: %+v", s)
		}
		if !authenticated && (s.Delivered != 0 || s.Dropped != 1) {
			t.Fatalf("missing client certificate accepted: %+v", s)
		}
	}
}
