package telemetry

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	collectorlogpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logpb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/goobers/goobers/internal/journal"
)

// Diagnostic transport limits bound queued memory and per-record work.
const (
	DiagnosticQueueLimit    = 128
	DiagnosticRecordLimit   = 64 << 10
	diagnosticExportTimeout = 2 * time.Second
)

// DiagnosticRecord is a short observation, not a synthetic workflow/span.
// Producers supply only documented operational fields, never arbitrary payloads.
type DiagnosticRecord struct {
	Time       time.Time
	Name       string
	Attributes map[string]any
}

// DiagnosticExportStats explicitly reports best-effort transport losses.
type DiagnosticExportStats struct {
	Accepted  uint64 `json:"accepted"`
	Delivered uint64 `json:"delivered"`
	Dropped   uint64 `json:"dropped"`
	Failures  uint64 `json:"failures"`
}

// DiagnosticExporter owns an independent, bounded OTLP Logs transport. A slow
// collector never blocks the producer or the journal/trace export stream.
type DiagnosticExporter struct {
	mu        sync.Mutex
	closed    bool
	queue     chan *collectorlogpb.ExportLogsServiceRequest
	done      chan struct{}
	cancel    context.CancelFunc
	conn      *grpc.ClientConn
	client    collectorlogpb.LogsServiceClient
	ctx       context.Context
	resource  *resourcepb.Resource
	scrubber  journal.Scrubber
	accepted  atomic.Uint64
	delivered atomic.Uint64
	dropped   atomic.Uint64
	failures  atomic.Uint64
}

// NewDiagnosticExporter reads no ambient OTLP variables. An empty endpoint
// creates no client, goroutine, DNS lookup, or connection.
func NewDiagnosticExporter(cfg Config) (*DiagnosticExporter, error) {
	if cfg.OTLPEndpoint == "" {
		return nil, nil
	}
	endpoint := cfg.OTLPEndpoint
	if strings.Contains(endpoint, "://") {
		u, err := url.Parse(endpoint)
		if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, errors.New("invalid diagnostic collector endpoint")
		}
		endpoint = u.Host
	}
	var transport credentials.TransportCredentials
	if cfg.OTLPInsecure {
		transport = insecure.NewCredentials()
	} else {
		tlsConfig, err := buildOTLPTLSConfig(cfg)
		if err != nil {
			return nil, fmt.Errorf("diagnostic collector TLS: %w", err)
		}
		transport = credentials.NewTLS(tlsConfig)
	}
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(transport))
	if err != nil {
		return nil, fmt.Errorf("create diagnostic collector: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &DiagnosticExporter{queue: make(chan *collectorlogpb.ExportLogsServiceRequest, DiagnosticQueueLimit), done: make(chan struct{}), cancel: cancel, conn: conn, client: collectorlogpb.NewLogsServiceClient(conn), scrubber: cfg.Scrubber}
	d.ctx = metadata.NewOutgoingContext(ctx, metadata.New(cfg.OTLPHeaders))
	d.resource = &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
		d.field("service.name", "goobers"), d.field("service.version", cfg.ServiceVersion), d.field("goobers.build.commit", cfg.BuildCommit), d.field("goobers.telemetry.stream", "diagnostics"),
	}}
	go d.run()
	return d, nil
}

func (d *DiagnosticExporter) field(key string, value any) *commonpb.KeyValue {
	v := new(commonpb.AnyValue)
	switch x := value.(type) {
	case bool:
		v.Value = &commonpb.AnyValue_BoolValue{BoolValue: x}
	case int64:
		v.Value = &commonpb.AnyValue_IntValue{IntValue: x}
	case int:
		v.Value = &commonpb.AnyValue_IntValue{IntValue: int64(x)}
	case float64:
		v.Value = &commonpb.AnyValue_DoubleValue{DoubleValue: x}
	default:
		s := fmt.Sprint(value)
		if d.scrubber != nil {
			s = string(d.scrubber.Scrub([]byte(s)))
		}
		v.Value = &commonpb.AnyValue_StringValue{StringValue: s}
	}
	return &commonpb.KeyValue{Key: key, Value: v}
}

// Emit takes an immutable snapshot and returns immediately. Oversize records,
// a full queue and shutdown are losses, counted rather than silently hidden.
func (d *DiagnosticExporter) Emit(record DiagnosticRecord) bool {
	if d == nil {
		return false
	}
	if record.Time.IsZero() || record.Name == "" || len(record.Name) > 256 || len(record.Attributes) > 64 {
		d.dropped.Add(1)
		return false
	}
	entry := &logpb.LogRecord{TimeUnixNano: nonNegativeUnixNano(record.Time), ObservedTimeUnixNano: nonNegativeUnixNano(time.Now()), Body: d.field("", record.Name).Value, SeverityNumber: logpb.SeverityNumber_SEVERITY_NUMBER_INFO}
	entry.Attributes = append(entry.Attributes, d.field("goobers.diagnostics.schema_version", 1))
	for key, value := range record.Attributes {
		if !validDiagnosticField(key, value) {
			d.dropped.Add(1)
			return false
		}
		entry.Attributes = append(entry.Attributes, d.field(key, value))
	}
	req := &collectorlogpb.ExportLogsServiceRequest{ResourceLogs: []*logpb.ResourceLogs{{Resource: d.resource, ScopeLogs: []*logpb.ScopeLogs{{Scope: &commonpb.InstrumentationScope{Name: "goobers.diagnostics", Version: "1"}, LogRecords: []*logpb.LogRecord{entry}}}}}}
	if proto.Size(req) > DiagnosticRecordLimit {
		d.dropped.Add(1)
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		d.dropped.Add(1)
		return false
	}
	select {
	case d.queue <- req:
		d.accepted.Add(1)
		return true
	default:
		d.dropped.Add(1)
		return false
	}
}

// Reject nested or unbounded payloads before formatting or copying them.
func validDiagnosticField(key string, value any) bool {
	if len(key) == 0 || len(key) > 128 || key == "goobers.diagnostics.schema_version" {
		return false
	}
	switch x := value.(type) {
	case string:
		return len(x) <= DiagnosticRecordLimit
	case bool, int, int64:
		return true
	case float64:
		return !math.IsNaN(x) && !math.IsInf(x, 0)
	default:
		return false
	}
}

func (d *DiagnosticExporter) run() {
	defer close(d.done)
	defer func() { _ = d.conn.Close() }()
	for req := range d.queue {
		if d.ctx.Err() != nil {
			d.dropped.Add(1)
			continue
		}
		ctx, cancel := context.WithTimeout(d.ctx, diagnosticExportTimeout)
		response, err := d.client.Export(ctx, req)
		cancel()
		if err != nil || (response.GetPartialSuccess().GetRejectedLogRecords() > 0) {
			d.failures.Add(1)
			d.dropped.Add(1)
		} else {
			d.delivered.Add(1)
		}
	}
}

// Stats returns transport counters; after Shutdown returns they are final.
func (d *DiagnosticExporter) Stats() DiagnosticExportStats {
	if d == nil {
		return DiagnosticExportStats{}
	}
	return DiagnosticExportStats{Accepted: d.accepted.Load(), Delivered: d.delivered.Load(), Dropped: d.dropped.Load(), Failures: d.failures.Load()}
}

// Shutdown drains within the caller's deadline, then cancels the outstanding
// RPC and drops queued records. Repeated/concurrent calls are safe.
func (d *DiagnosticExporter) Shutdown(ctx context.Context) error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	if !d.closed {
		d.closed = true
		close(d.queue)
	}
	d.mu.Unlock()
	select {
	case <-d.done:
		d.cancel()
		return nil
	case <-ctx.Done():
		d.cancel()
		// gRPC observes cancellation; the bounded queue is then accounted as
		// dropped without further RPCs. Final statistics are stable on return.
		<-d.done
		return ctx.Err()
	}
}
