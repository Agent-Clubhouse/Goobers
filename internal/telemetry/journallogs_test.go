package telemetry

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	collectorlogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/goobers/goobers/internal/journal"
)

type journalLogsReceiver struct {
	collectorlogspb.UnimplementedLogsServiceServer
	requests chan *collectorlogspb.ExportLogsServiceRequest
	headers  chan metadata.MD
}

func (s *journalLogsReceiver) Export(ctx context.Context, req *collectorlogspb.ExportLogsServiceRequest) (*collectorlogspb.ExportLogsServiceResponse, error) {
	s.requests <- req
	md, _ := metadata.FromIncomingContext(ctx)
	s.headers <- md
	return &collectorlogspb.ExportLogsServiceResponse{}, nil
}

func TestJournalLogsOTLPWireContract(t *testing.T) {
	t.Run("full-client", func(t *testing.T) { testJournalLogsOTLPWireContract(t, false) })
	t.Run("logs-only", func(t *testing.T) { testJournalLogsOTLPWireContract(t, true) })
}

func testJournalLogsOTLPWireContract(t *testing.T, logsOnly bool) {
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
	t.Cleanup(server.Stop)
	registry, scrubber := journal.DefaultScrubber()
	registry.Register([]byte("journal-resource-secret"))
	cfg := Config{
		Exporter: ExporterOTLP, OTLPEndpoint: "http://" + listener.Addr().String(), OTLPInsecure: true,
		OTLPHeaders: map[string]string{"x-journal-test": "present"}, JournalLogs: true,
		JournalLogsOnly: logsOnly,
		ServiceName:     "journal-test", ServiceVersion: "test-version", Environment: "test",
		Scrubber: scrubber,
		ResourceAttributes: []attribute.KeyValue{
			attribute.String("test.resource", "retained"),
			attribute.String("test.secret", "journal-resource-secret"),
		},
	}
	client, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if logsOnly && (client.tracerProvider != nil || client.meterProvider != nil || client.instruments != nil || client.localSpanProcessor != nil) {
		t.Fatal("logs-only client constructed trace or metric providers")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = client.Shutdown(ctx)
	})
	body := []byte(`{"seq":18446744073709551615,"message":"[REDACTED]","data":{"integer":9007199254740993}}`)
	event := journal.CommittedEvent{
		Kind: "run", JournalID: "stable-journal", InstanceID: "instance", Gaggle: "gaggle",
		RunID: "0123456789abcdef0123456789abcdef", Seq: math.MaxUint64,
		Time: time.Unix(1700000000, 123), ObservedTime: time.Unix(1700000001, 456), Body: body,
	}
	client.Commit(event)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.journalLogs.flush(ctx); err != nil {
		t.Fatal(err)
	}
	var request *collectorlogspb.ExportLogsServiceRequest
	select {
	case request = <-receiver.requests:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	rs := request.ResourceLogs[0]
	resourceAttrs := journalWireAttrs(rs.Resource.Attributes)
	if resourceAttrs["service.name"].GetStringValue() != cfg.ServiceName ||
		resourceAttrs["service.version"].GetStringValue() != cfg.ServiceVersion ||
		resourceAttrs["test.resource"].GetStringValue() != "retained" ||
		resourceAttrs["service.instance.id"].GetStringValue() == "" {
		t.Fatalf("resource = %v", rs.Resource)
	}
	if got := resourceAttrs["test.secret"].GetStringValue(); got != string(scrubber.Scrub([]byte("journal-resource-secret"))) {
		t.Fatalf("resource not scrubbed: %q", got)
	}
	scope := rs.ScopeLogs[0]
	if scope.Scope.Name != "goobers.journal" || scope.Scope.Version != "1" {
		t.Fatalf("scope = %v", scope.Scope)
	}
	record := scope.LogRecords[0]
	if record.Body.GetStringValue() != string(body) {
		t.Fatalf("body changed: %q", record.Body.GetStringValue())
	}
	if record.TimeUnixNano != uint64(event.Time.UnixNano()) || record.ObservedTimeUnixNano != uint64(event.ObservedTime.UnixNano()) {
		t.Fatalf("timestamps = %v", record)
	}
	if !bytes.Equal(record.TraceId, []byte{1, 35, 69, 103, 137, 171, 205, 239, 1, 35, 69, 103, 137, 171, 205, 239}) || len(record.SpanId) != 0 {
		t.Fatalf("correlation = trace %x span %x", record.TraceId, record.SpanId)
	}
	attrs := journalWireAttrs(record.Attributes)
	if attrs["goobers.journal.schema_version"].GetIntValue() != 1 ||
		attrs["goobers.journal.seq"].GetStringValue() != "18446744073709551615" ||
		attrs["goobers.journal.kind"].GetStringValue() != "run" ||
		attrs["goobers.journal.id"].GetStringValue() != event.JournalID ||
		attrs["goobers.instance.id"].GetStringValue() != event.InstanceID ||
		attrs["goobers.gaggle"].GetStringValue() != event.Gaggle ||
		attrs["goobers.run.id"].GetStringValue() != event.RunID || len(attrs) != 7 {
		t.Fatalf("attributes = %v", attrs)
	}
	if got := (<-receiver.headers).Get("x-journal-test"); len(got) != 1 || got[0] != "present" {
		t.Fatalf("headers = %v", got)
	}
	event.Kind, event.RunID, event.InstanceID, event.Gaggle = "scheduler", "", "", ""
	client.Commit(event)
	if err := client.journalLogs.flush(ctx); err != nil {
		t.Fatal(err)
	}
	record = (<-receiver.requests).ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	if len(record.TraceId) != 0 || len(record.SpanId) != 0 || len(record.Attributes) != 4 {
		t.Fatalf("scheduler metadata = %v", record)
	}
	if stats := client.JournalExportStats(); stats.Accepted != 2 || stats.Dropped != 0 || stats.ExportFailures != 0 {
		t.Fatalf("stats = %+v", stats)
	}
}

func journalWireAttrs(attrs []*commonpb.KeyValue) map[string]*commonpb.AnyValue {
	result := make(map[string]*commonpb.AnyValue, len(attrs))
	for _, kv := range attrs {
		result[kv.Key] = kv.Value
	}
	return result
}

type journalTestExporter struct {
	mu          sync.Mutex
	records     []sdklog.Record
	started     chan struct{}
	release     chan struct{}
	startOnce   sync.Once
	exportErr   error
	flushErr    error
	shutdownErr error
	flushes     int
	shutdowns   int
}

func (e *journalTestExporter) Export(ctx context.Context, records []sdklog.Record) error {
	if e.started != nil {
		e.startOnce.Do(func() { close(e.started) })
	}
	if e.release != nil {
		select {
		case <-e.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range records {
		e.records = append(e.records, r.Clone())
	}
	return e.exportErr
}

func (e *journalTestExporter) ForceFlush(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.flushes++
	return e.flushErr
}

func (e *journalTestExporter) Shutdown(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.shutdowns++
	return e.shutdownErr
}

func journalTestClient(t *testing.T, exporter sdklog.Exporter) *Client {
	t.Helper()
	client := &Client{journalLogs: newJournalLogPipeline(exporter, resource.Empty(), nil)}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = client.journalLogs.shutdown(ctx)
	})
	return client
}

func waitJournalStarted(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("export did not start")
	}
}

type panickingJournalStatsSink struct{}

func (panickingJournalStatsSink) Commit(journal.CommittedEvent) {
	panic("synthetic journal sink failure")
}

func TestJournalLogsStatsExposeProcessSinkPanics(t *testing.T) {
	client := journalTestClient(t, &journalTestExporter{})
	before := client.JournalExportStats()
	root := t.TempDir()
	unregister, err := journal.RegisterCommittedEventSink(root, "test-instance", panickingJournalStatsSink{})
	if err != nil {
		t.Fatal(err)
	}
	defer unregister()
	log, _, err := journal.OpenInstanceLog(filepath.Join(root, "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := log.Close(); err != nil {
			t.Errorf("close scheduler journal: %v", err)
		}
	}()
	if err := log.Append(journal.Event{Type: journal.EventRunnerAnnotation}); err != nil {
		t.Fatal(err)
	}
	after := client.JournalExportStats()
	if after.SinkPanics != before.SinkPanics+1 {
		t.Fatalf("sink panics = %d, want %d", after.SinkPanics, before.SinkPanics+1)
	}
	if after.Dropped != before.Dropped || after.ExportFailures != before.ExportFailures {
		t.Fatalf("another sink's panic changed client queue accounting: before=%+v after=%+v", before, after)
	}
}

func TestJournalLogsQueueBoundsAndOwnership(t *testing.T) {
	for _, test := range []struct {
		name string
		body int
	}{
		{"count", 1},
		{"bytes", journalLogRecordLimit / 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			exporter := &journalTestExporter{started: make(chan struct{}), release: make(chan struct{})}
			client := journalTestClient(t, exporter)
			body := bytes.Repeat([]byte("x"), test.body)
			event := journal.CommittedEvent{Body: body, Kind: "run", JournalID: "id"}
			client.Commit(event)
			waitJournalStarted(t, exporter.started)
			body[0] = 'z'
			for i := 0; i < journalLogQueueLimit+10; i++ {
				client.Commit(event)
			}
			stats := client.JournalExportStats()
			if stats.Dropped == 0 || stats.QueuedRecords > journalLogQueueLimit ||
				stats.QueuedBytes > journalLogBytesLimit ||
				stats.Accepted+stats.Dropped != journalLogQueueLimit+11 {
				t.Fatalf("queue accounting = %+v", stats)
			}
			if test.name == "count" && stats.QueuedRecords != journalLogQueueLimit {
				t.Fatalf("item bound not reached: %+v", stats)
			}
			if test.name == "bytes" && stats.QueuedRecords >= journalLogQueueLimit {
				t.Fatalf("byte bound not applied: %+v", stats)
			}
			close(exporter.release)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := client.journalLogs.flush(ctx); err != nil {
				t.Fatal(err)
			}
			stats = client.JournalExportStats()
			if stats.QueuedBytes != 0 || stats.QueuedRecords != 0 {
				t.Fatalf("queue not drained: %+v", stats)
			}
			exporter.mu.Lock()
			defer exporter.mu.Unlock()
			if got := exporter.records[0].Body().AsString(); got[0] != 'x' {
				t.Fatal("queued body was not copied")
			}
		})
	}
}

func TestJournalLogsRejectLargeRecordsAndContention(t *testing.T) {
	client := journalTestClient(t, &journalTestExporter{})
	client.Commit(journal.CommittedEvent{JournalID: "test-journal", Body: make([]byte, journalLogRecordLimit+1)})
	client.Commit(journal.CommittedEvent{JournalID: strings.Repeat("x", journalLogRecordLimit+1)})
	client.journalLogs.mu.Lock()
	// This must return without waiting for the worker's mutex.
	client.Commit(journal.CommittedEvent{JournalID: "test-journal", Body: []byte("{}")})
	client.journalLogs.mu.Unlock()
	stats := client.JournalExportStats()
	if stats.Accepted != 0 || stats.Dropped != 3 || stats.QueuedBytes != 0 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestJournalLogsRejectsMissingJournalIdentity(t *testing.T) {
	exporter := &journalTestExporter{}
	client := journalTestClient(t, exporter)
	reports := make(chan string, 8)
	client.journalLogs.reporter.log = func(_ string, args ...any) {
		for i := 0; i+1 < len(args); i += 2 {
			if args[i] == "error" {
				reports <- args[i+1].(string)
			}
		}
	}
	// Invalid metadata must be rejected without waiting on even the queue lock.
	client.journalLogs.mu.Lock()
	client.Commit(journal.CommittedEvent{Kind: "scheduler", Body: []byte("{}")})
	client.journalLogs.mu.Unlock()
	stats := client.JournalExportStats()
	if stats.Accepted != 0 || stats.Dropped != 1 || stats.InvalidMetadata != 1 || stats.QueuedBytes != 0 {
		t.Fatalf("missing identity accounting = %+v", stats)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for {
		select {
		case message := <-reports:
			if strings.Contains(message, "missing persistent journal identity") {
				return
			}
		case <-ctx.Done():
			t.Fatal("missing identity was not reported by the worker")
		}
	}
}

func TestJournalLogsFlushShutdownTimeout(t *testing.T) {
	exporter := &journalTestExporter{started: make(chan struct{}), release: make(chan struct{})}
	client := journalTestClient(t, exporter)
	client.Commit(journal.CommittedEvent{JournalID: "test-journal", Body: []byte("{}")})
	waitJournalStarted(t, exporter.started)
	client.Commit(journal.CommittedEvent{JournalID: "test-journal", Body: []byte("{}")})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := client.journalLogs.flush(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("flush = %v", err)
	}
	if err := client.journalLogs.shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown = %v", err)
	}
	select {
	case <-client.journalLogs.done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after cancellation")
	}
	client.Commit(journal.CommittedEvent{JournalID: "test-journal"})
	stats := client.JournalExportStats()
	if stats.ExportFailures != 1 || stats.Dropped != 2 || stats.QueuedRecords != 0 || stats.QueuedBytes != 0 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestJournalLogsExportAndLifecycleErrors(t *testing.T) {
	exporter := &journalTestExporter{
		exportErr: errors.New("collector unavailable"), flushErr: errors.New("flush failed"),
		shutdownErr: errors.New("shutdown failed"),
	}
	client := journalTestClient(t, exporter)
	client.Commit(journal.CommittedEvent{JournalID: "test-journal", Body: []byte("{}")})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.journalLogs.flush(ctx); !errors.Is(err, exporter.flushErr) {
		t.Fatalf("flush = %v", err)
	}
	if stats := client.JournalExportStats(); stats.ExportFailures != 1 || stats.Accepted != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if err := client.journalLogs.shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.journalLogs.shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	exporter.mu.Lock()
	defer exporter.mu.Unlock()
	if exporter.shutdowns != 1 || exporter.flushes == 0 {
		t.Fatalf("flushes=%d shutdowns=%d", exporter.flushes, exporter.shutdowns)
	}
}

func TestJournalLogsClientLifecycleRemainsBestEffort(t *testing.T) {
	client, err := New(context.Background(), Config{Stdout: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	client.journalLogs = newJournalLogPipeline(&journalTestExporter{
		exportErr:   errors.New("collector unavailable"),
		flushErr:    errors.New("collector flush failed"),
		shutdownErr: errors.New("collector shutdown failed"),
	}, resource.Empty(), nil)
	client.Commit(journal.CommittedEvent{JournalID: "test-journal", Body: []byte("{}")})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Flush(ctx); err != nil {
		t.Fatalf("remote logs failure escaped Client.Flush: %v", err)
	}
	if err := client.Shutdown(ctx); err != nil {
		t.Fatalf("remote logs failure escaped Client.Shutdown: %v", err)
	}
	if stats := client.JournalExportStats(); stats.ExportFailures != 1 {
		t.Fatalf("remote failure was not counted: %+v", stats)
	}
}

func TestJournalLogsShutdownDrainsAndInvalidRunIDStaysMetadata(t *testing.T) {
	exporter := &journalTestExporter{started: make(chan struct{}), release: make(chan struct{})}
	client := journalTestClient(t, exporter)
	event := journal.CommittedEvent{JournalID: "test-journal", RunID: "not-a-trace-id", Kind: "scheduler", Body: []byte("{}")}
	client.Commit(event)
	waitJournalStarted(t, exporter.started)
	for range 10 {
		client.Commit(event)
	}
	close(exporter.release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.journalLogs.shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if stats := client.JournalExportStats(); stats.Accepted != 11 || stats.Dropped != 0 || stats.QueuedRecords != 0 {
		t.Fatalf("drain stats = %+v", stats)
	}
	exporter.mu.Lock()
	defer exporter.mu.Unlock()
	if len(exporter.records) != 11 || exporter.shutdowns != 1 {
		t.Fatalf("exported=%d shutdowns=%d", len(exporter.records), exporter.shutdowns)
	}
	for _, record := range exporter.records {
		if record.TraceID().IsValid() || record.SpanID().IsValid() {
			t.Fatalf("fabricated correlation: %v", record)
		}
		var runID string
		record.WalkAttributes(func(kv attribute.KeyValue) bool {
			if kv.Key == "goobers.run.id" {
				runID = kv.Value.AsString()
			}
			return true
		})
		if runID != event.RunID {
			t.Fatalf("run ID metadata = %q", runID)
		}
	}
}

func TestJournalLogsDisabledAndDegraded(t *testing.T) {
	for _, cfg := range []Config{
		{Stdout: io.Discard},
		{Stdout: io.Discard, JournalLogs: true},
		{Exporter: ExporterOTLP, OTLPEndpoint: "127.0.0.1:1", OTLPInsecure: true},
	} {
		client, err := New(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}

		client.Commit(journal.CommittedEvent{Body: []byte("{}")})
		if client.JournalLogsEnabled() || client.JournalExportStats() != (JournalExportStats{}) {
			t.Fatal("disabled logs pipeline was created")
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		_ = client.Shutdown(ctx)
		cancel()
	}
	if client, err := New(context.Background(), Config{Exporter: ExporterOTLP, JournalLogs: true}); err == nil || client != nil {
		t.Fatalf("OTLP without endpoint = %v, %v", client, err)
	}
	client, err := New(context.Background(), Config{
		Exporter: ExporterOTLP, OTLPEndpoint: "localhost:4317", JournalLogs: true,
		OTLPCAFile: "does-not-exist-journal-ca.pem",
	})
	if !errors.Is(err, ErrOTLPUnavailable) || client == nil || client.journalLogs != nil {
		t.Fatalf("degraded = %v, %v", client, err)
	}
	if err := client.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	var nilClient *Client
	nilClient.Commit(journal.CommittedEvent{})
	if nilClient.JournalLogsEnabled() || nilClient.JournalExportStats() != (JournalExportStats{}) {
		t.Fatal("nil client has stats")
	}
}

func TestJournalLogsOnlyDisabledAndDegraded(t *testing.T) {
	for _, cfg := range []Config{
		{JournalLogsOnly: true},
		{JournalLogsOnly: true, JournalLogs: true, Exporter: ExporterStdout},
		{JournalLogsOnly: true, JournalLogs: true, Exporter: ExporterOTLP},
		{JournalLogsOnly: true, JournalLogs: false, Exporter: ExporterOTLP, OTLPEndpoint: "127.0.0.1:1"},
	} {
		var stdout bytes.Buffer
		cfg.Stdout = &stdout
		client, err := New(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if client.JournalLogsEnabled() || client.tracerProvider != nil || client.meterProvider != nil {
			t.Fatal("disabled logs-only client constructed providers")
		}
		if err := client.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := client.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		if stdout.Len() != 0 {
			t.Fatal("logs-only client wrote stdout telemetry")
		}
	}
	client, err := New(context.Background(), Config{
		JournalLogsOnly: true, JournalLogs: true, Exporter: ExporterOTLP,
		OTLPEndpoint: "127.0.0.1:1", OTLPCAFile: "does-not-exist-journal-ca.pem",
	})
	if !errors.Is(err, ErrOTLPUnavailable) || client == nil || client.JournalLogsEnabled() {
		t.Fatalf("degraded logs-only client = %v, %v", client, err)
	}
	if err := client.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestJournalLogsProviderOwnsRegistration(t *testing.T) {
	id, err := NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	root := ".journal-registration-" + id
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	cfg := Config{
		Exporter: ExporterOTLP, OTLPEndpoint: "127.0.0.1:1", OTLPInsecure: true,
		JournalLogs: true, JournalRoot: root, JournalInstanceID: "known-instance",
	}
	client, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = client.Shutdown(ctx)
	})
	if !journal.HasCommittedEventSink(root) {
		t.Fatal("client did not register its sink")
	}
	if !client.JournalLogsEnabled() {
		t.Fatal("registered client's logs are not enabled")
	}

	duplicate, err := New(context.Background(), cfg)
	if !errors.Is(err, ErrOTLPUnavailable) || duplicate == nil || duplicate.JournalLogsEnabled() {
		t.Fatalf("overlapping registration = %v, %v", duplicate, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = duplicate.Shutdown(ctx)
	if !journal.HasCommittedEventSink(root) {
		t.Fatal("degraded duplicate removed the existing owner")
	}
	_ = client.Shutdown(ctx)
	if journal.HasCommittedEventSink(root) {
		t.Fatal("cancelled shutdown did not unregister first")
	}
	if client.JournalLogsEnabled() {
		t.Fatal("shutdown client reports enabled journal logs")
	}
	_ = client.Shutdown(ctx)

	cfg.JournalRoot = filepath.Join(root, "does-not-exist")
	degraded, err := New(context.Background(), cfg)
	if !errors.Is(err, ErrOTLPUnavailable) || degraded == nil || degraded.journalLogs != nil {
		t.Fatalf("invalid registration root = %v, %v", degraded, err)
	}
	_ = degraded.Shutdown(ctx)

	cfg.JournalLogs = false
	disabled, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("disabled logs inspected the registration root: %v", err)
	}
	_ = disabled.Shutdown(ctx)
}

// TestJournalLogsShutdownDeadlineAccountsAbandonedBacklog pins the accounting a
// short-lived CLI depends on: it reads JournalExportStats as soon as Shutdown
// returns. If shutdown leaves the abandoned backlog for the worker to settle,
// that read reports Dropped: 0 for records that were never sent, and the
// process exits before the worker's warning is ever printed.
func TestJournalLogsShutdownDeadlineAccountsAbandonedBacklog(t *testing.T) {
	exporter := &journalTestExporter{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	client := &Client{journalLogs: newJournalLogPipeline(exporter, resource.Empty(), nil)}

	// The first record parks the worker inside Export; the rest queue behind it.
	const queued = 6
	for range queued {
		client.Commit(journal.CommittedEvent{JournalID: "test-journal", Body: []byte("{}")})
	}
	<-exporter.started

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := client.journalLogs.shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error = %v; want context.DeadlineExceeded", err)
	}

	stats := client.JournalExportStats()
	if want := uint64(queued - 1); stats.Dropped != want {
		t.Fatalf("Dropped = %d immediately after shutdown; want %d (the queued records the deadline abandoned)",
			stats.Dropped, want)
	}
	close(exporter.release)
}

// TestJournalLogsAttributesDropsToDistinctCauses pins the #5573 breakdown: a
// single Dropped total cannot tell "the collector is not keeping up" from
// "this record is too big", and those have different fixes.
func TestJournalLogsAttributesDropsToDistinctCauses(t *testing.T) {
	exporter := &journalTestExporter{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	client := &Client{journalLogs: newJournalLogPipeline(exporter, resource.Empty(), nil)}
	defer close(exporter.release)

	// Missing identity.
	client.Commit(journal.CommittedEvent{Body: []byte("{}")})
	// Oversized record, which must not be charged to queue_full.
	client.Commit(journal.CommittedEvent{
		JournalID: "test-journal",
		Body:      make([]byte, journalLogRecordLimit+1),
	})
	// Park the worker inside Export before filling the ring so scheduling
	// cannot drain records while this test is creating queue_full drops.
	client.Commit(journal.CommittedEvent{JournalID: "test-journal", Body: []byte("{}")})
	<-exporter.started
	for range journalLogQueueLimit + 5 {
		client.Commit(journal.CommittedEvent{JournalID: "test-journal", Body: []byte("{}")})
	}

	stats := client.JournalExportStats()
	if stats.InvalidMetadata != 1 {
		t.Errorf("InvalidMetadata = %d; want 1", stats.InvalidMetadata)
	}
	if stats.DroppedRecordTooLarge != 1 {
		t.Errorf("DroppedRecordTooLarge = %d; want 1", stats.DroppedRecordTooLarge)
	}
	if stats.DroppedQueueFull == 0 {
		t.Error("DroppedQueueFull = 0; want the overflow charged to queue_full, not folded into a single total")
	}
	// An oversized record is not overload, and overload is not bad metadata.
	sum := stats.InvalidMetadata + stats.DroppedRecordTooLarge + stats.DroppedLockContention +
		stats.DroppedQueueFull + stats.DroppedStopping + stats.DroppedShutdown
	if sum != stats.Dropped {
		t.Errorf("per-cause counts sum to %d; want Dropped = %d", sum, stats.Dropped)
	}
}

// TestJournalLogsShutdownDropsAreChargedToShutdown keeps the abandoned backlog
// distinguishable from steady-state overload.
func TestJournalLogsShutdownDropsAreChargedToShutdown(t *testing.T) {
	exporter := &journalTestExporter{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	client := &Client{journalLogs: newJournalLogPipeline(exporter, resource.Empty(), nil)}

	const queued = 6
	for range queued {
		client.Commit(journal.CommittedEvent{JournalID: "test-journal", Body: []byte("{}")})
	}
	<-exporter.started

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := client.journalLogs.shutdown(ctx); err == nil {
		t.Fatal("shutdown returned nil; want a deadline error with records still queued")
	}
	stats := client.JournalExportStats()
	if want := uint64(queued - 1); stats.DroppedShutdown != want {
		t.Fatalf("DroppedShutdown = %d; want %d", stats.DroppedShutdown, want)
	}
	if stats.DroppedQueueFull != 0 {
		t.Fatalf("DroppedQueueFull = %d; abandoned records are not overload", stats.DroppedQueueFull)
	}
	close(exporter.release)
}

// TestClientShutdownExportsFinalJournalDropCauses exercises the real Client
// lifecycle boundary. The journal exporter is held past the shutdown deadline
// so shutdown itself creates both stopping and abandoned-backlog drops; the
// metric exporter must observe those causes before its provider is closed.
func TestClientShutdownExportsFinalJournalDropCauses(t *testing.T) {
	metricExporter := &countingMetricExporter{}
	client := newMetricsClient(t, Config{
		MetricExporter:       metricExporter,
		MetricExportInterval: time.Hour,
	})
	logExporter := &journalTestExporter{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	defer close(logExporter.release)
	client.journalLogs = newJournalLogPipeline(logExporter, resource.Empty(), nil)
	client.journalLogs.observeDrops = client.journalExportDropped

	const accepted = 4
	for range accepted {
		client.Commit(journal.CommittedEvent{JournalID: "test-journal", Body: []byte("{}")})
	}
	<-logExporter.started

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.Shutdown(ctx) }()

	deadline := time.Now().Add(time.Second)
	for {
		client.journalLogs.mu.Lock()
		stopping := client.journalLogs.stopping
		client.journalLogs.mu.Unlock()
		if stopping {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("journal pipeline never entered stopping state")
		}
		time.Sleep(time.Millisecond)
	}
	client.Commit(journal.CommittedEvent{JournalID: "after-stop", Body: []byte("{}")})

	if err := <-done; err != nil {
		t.Fatalf("Client.Shutdown: %v", err)
	}
	counts := metricExporter.journalDropCounts()
	if got := counts[dropStopping.String()]; got != 1 {
		t.Errorf("stopping metric = %d, want 1 (all causes: %v)", got, counts)
	}
	if got, want := counts[dropShutdown.String()], int64(accepted-1); got != want {
		t.Errorf("shutdown metric = %d, want %d (all causes: %v)", got, want, counts)
	}
}
