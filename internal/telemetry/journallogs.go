package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	apilog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	journalLogQueueLimit  = 1024
	journalLogBytesLimit  = 8 << 20
	journalLogRecordLimit = 1 << 20
	journalLogTimeout     = 5 * time.Second
)

// JournalExportStats is a process-local snapshot. Accepted counts admitted
// records, not collector acknowledgements. Dropped includes overload and records
// abandoned when shutdown expires. ExportFailures counts failed export calls.
type JournalExportStats struct {
	Accepted       uint64
	Dropped        uint64
	ExportFailures uint64
	// InvalidMetadata counts dropped records missing the persistent journal ID.
	InvalidMetadata uint64
	QueuedRecords   int
	QueuedBytes     int
}

var _ journal.CommittedEventSink = (*Client)(nil)

func (c *Client) configureJournalLogs(ctx context.Context, cfg Config, res *resource.Resource) error {
	if !cfg.JournalLogs || cfg.Exporter != ExporterOTLP || strings.TrimSpace(cfg.OTLPEndpoint) == "" {
		return nil
	}
	exporter, err := newJournalLogExporter(ctx, cfg)
	if err != nil {
		return fmt.Errorf("%w: create journal logs exporter: %w", ErrOTLPUnavailable, err)
	}
	c.journalLogs = newJournalLogPipeline(exporter, res)
	if cfg.JournalRoot == "" {
		return nil
	}
	unregister, err := journal.RegisterCommittedEventSink(cfg.JournalRoot, cfg.JournalInstanceID, c)
	if err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), journalLogTimeout)
		defer cancel()
		_ = c.journalLogs.shutdown(shutdownCtx)
		c.journalLogs = nil
		return fmt.Errorf("%w: register journal logs: %w", ErrOTLPUnavailable, err)
	}
	c.unregisterJournal = unregister
	return nil
}

// Commit only copies into a bounded queue. It never exports or logs while the
// caller holds a journal lock. The journal remains authoritative on overload.
func (c *Client) Commit(event journal.CommittedEvent) {
	if c != nil && c.journalLogs != nil {
		c.journalLogs.commit(event)
	}
}

// JournalLogsEnabled reports whether the client has a live journal Logs queue.
// Disabled, degraded, nil, and shutting-down clients return false.
func (c *Client) JournalLogsEnabled() bool {
	if c == nil || c.journalLogs == nil {
		return false
	}
	p := c.journalLogs
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.stopping
}

// JournalExportStats returns zero values when live journal export is disabled.
func (c *Client) JournalExportStats() JournalExportStats {
	if c == nil || c.journalLogs == nil {
		return JournalExportStats{}
	}
	p := c.journalLogs
	p.mu.Lock()
	defer p.mu.Unlock()
	return JournalExportStats{
		Accepted: p.accepted.Load(), Dropped: p.dropped.Load(),
		ExportFailures: p.failures.Load(), QueuedRecords: p.pending, QueuedBytes: p.bytes,
		InvalidMetadata: p.invalidMetadata.Load(),
	}
}

type journalLogItem struct {
	event journal.CommittedEvent
	bytes int
}

type journalLogFlush struct {
	ctx  context.Context
	done chan error
}

type journalLogPipeline struct {
	mu              sync.Mutex
	queue           [journalLogQueueLimit]journalLogItem
	head            int
	length          int
	pending         int // Includes the single in-flight record.
	bytes           int // Includes the single in-flight record and all metadata.
	stopping        bool
	progress        chan struct{}
	wake            chan struct{}
	done            chan struct{}
	ready           chan struct{}
	flushes         chan journalLogFlush
	ctx             context.Context
	cancel          context.CancelFunc
	provider        *sdklog.LoggerProvider
	logger          apilog.Logger
	reporter        *exportErrorHandler
	accepted        atomic.Uint64
	dropped         atomic.Uint64
	failures        atomic.Uint64
	invalidMetadata atomic.Uint64
	completed       atomic.Uint64
}

func newJournalLogPipeline(exporter sdklog.Exporter, res *resource.Resource) *journalLogPipeline {
	ctx, cancel := context.WithCancel(context.Background())
	p := &journalLogPipeline{
		ctx: ctx, cancel: cancel, progress: make(chan struct{}),
		wake: make(chan struct{}, 1), done: make(chan struct{}), ready: make(chan struct{}),
		flushes:  make(chan journalLogFlush, 1),
		reporter: newExportErrorHandler(),
	}
	// The application queue is the only lossy boundary. A batch processor here
	// would introduce another queue whose losses could not be accounted for.
	p.provider = sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithAttributeCountLimit(8),
		sdklog.WithAttributeValueLengthLimit(-1),
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(&journalLogExporter{Exporter: exporter, pipeline: p})),
	)
	p.logger = p.provider.Logger("goobers.journal", apilog.WithInstrumentationVersion("1"))
	go p.run()
	<-p.ready
	return p
}

func (p *journalLogPipeline) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *journalLogPipeline) drop() {
	p.dropped.Add(1)
	p.signal()
}

func journalLogSize(e journal.CommittedEvent) int {
	// All variable-sized data retained by a queued event is charged, including
	// metadata. The fixed-size ring is independently bounded by item count.
	return len(e.Body) + len(e.Kind) + len(e.JournalID) + len(e.InstanceID) + len(e.Gaggle) + len(e.RunID)
}

func (p *journalLogPipeline) commit(e journal.CommittedEvent) {
	if e.JournalID == "" {
		p.invalidMetadata.Add(1)
		p.drop()
		return
	}
	size := journalLogSize(e)
	if size > journalLogRecordLimit || !p.mu.TryLock() {
		p.drop()
		return
	}
	if p.stopping || p.pending >= journalLogQueueLimit || size > journalLogBytesLimit-p.bytes {
		p.mu.Unlock()
		p.drop()
		return
	}
	// Clone even owned data to avoid retaining oversized backing allocations
	// from callers. Copies are bounded before allocation.
	e.Body = append([]byte(nil), e.Body...)
	e.Kind = strings.Clone(e.Kind)
	e.JournalID = strings.Clone(e.JournalID)
	e.InstanceID = strings.Clone(e.InstanceID)
	e.Gaggle = strings.Clone(e.Gaggle)
	e.RunID = strings.Clone(e.RunID)
	p.queue[(p.head+p.length)%len(p.queue)] = journalLogItem{event: e, bytes: size}
	p.length++
	p.pending++
	p.bytes += size
	p.accepted.Add(1)
	p.mu.Unlock()
	p.signal()
}

func (p *journalLogPipeline) run() {
	defer close(p.done)
	defer p.cancel()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), journalLogTimeout)
		defer cancel()
		if err := p.provider.Shutdown(ctx); err != nil {
			p.reporter.Handle(fmt.Errorf("journal logs shutdown: %w", err))
		}
	}()
	var reportedDrops uint64
	var reportedInvalidMetadata uint64
	started := false
	for {
		if invalid := p.invalidMetadata.Load(); invalid != reportedInvalidMetadata {
			p.reporter.Handle(errors.New("journal logs dropped: missing persistent journal identity; inspect JournalExportStats.InvalidMetadata"))
			reportedInvalidMetadata = invalid
		}
		select {
		case request := <-p.flushes:
			err := p.provider.ForceFlush(request.ctx)
			if err != nil {
				p.reporter.Handle(fmt.Errorf("journal logs flush: %w", err))
			}
			request.done <- err
		default:
		}
		if drops := p.dropped.Load(); drops != reportedDrops {
			// Keep one stable signature so the existing rate limiter bounds
			// repeated overflow notices. Exact totals remain in the stats API.
			p.reporter.Handle(errors.New("journal logs dropped: invalid metadata, queue full, record too large, or exporter stopping; inspect JournalExportStats"))
			reportedDrops = drops
		}
		p.mu.Lock()
		if p.ctx.Err() != nil {
			p.dropped.Add(uint64(p.length))
			clear(p.queue[:])
			p.length, p.pending, p.bytes = 0, 0, 0
			close(p.progress)
			p.progress = make(chan struct{})
			p.mu.Unlock()
			p.reporter.Handle(errors.New("journal logs shutdown deadline expired; pending records dropped"))
			return
		}
		if p.length == 0 {
			stopping := p.stopping
			p.mu.Unlock()
			if !started {
				// Publish readiness only after releasing the queue lock. The
				// first committed event must not race worker initialization.
				close(p.ready)
				started = true
			}
			if stopping {
				return
			}
			select {
			case <-p.wake:
			case <-p.ctx.Done():
			case request := <-p.flushes:
				err := p.provider.ForceFlush(request.ctx)
				if err != nil {
					p.reporter.Handle(fmt.Errorf("journal logs flush: %w", err))
				}
				request.done <- err
			}
			continue
		}
		item := p.queue[p.head]
		p.queue[p.head] = journalLogItem{}
		p.head = (p.head + 1) % len(p.queue)
		p.length--
		p.mu.Unlock()

		p.emit(item.event)

		p.mu.Lock()
		p.pending--
		p.bytes -= item.bytes
		p.completed.Add(1)
		close(p.progress)
		p.progress = make(chan struct{})
		p.mu.Unlock()
	}
}

func (p *journalLogPipeline) emit(e journal.CommittedEvent) {
	var record apilog.Record
	record.SetTimestamp(e.Time)
	record.SetObservedTimestamp(e.ObservedTime)
	record.SetBody(attribute.StringValue(string(e.Body)))
	record.AddAttributes(
		attribute.Int("goobers.journal.schema_version", 1),
		attribute.String("goobers.journal.kind", e.Kind),
		attribute.String("goobers.journal.id", e.JournalID),
		attribute.String("goobers.journal.seq", strconv.FormatUint(e.Seq, 10)),
	)
	for _, kv := range []attribute.KeyValue{
		attribute.String("goobers.instance.id", e.InstanceID),
		attribute.String("goobers.gaggle", e.Gaggle),
		attribute.String("goobers.run.id", e.RunID),
	} {
		if kv.Value.AsString() != "" {
			record.AddAttributes(kv)
		}
	}
	ctx, cancel := context.WithTimeout(p.ctx, journalLogTimeout)
	defer cancel()
	if id, err := trace.TraceIDFromHex(e.RunID); err == nil && id.IsValid() {
		// The journal knows the run trace, not an active span. Trace-only
		// correlation is intentional; manufacturing a span ID is incorrect.
		ctx = trace.ContextWithSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{TraceID: id}))
	}
	p.logger.Emit(ctx, record)
}

func (p *journalLogPipeline) flush(ctx context.Context) error {
	target := p.accepted.Load()
	for {
		p.mu.Lock()
		completed, progress := p.completed.Load(), p.progress
		p.mu.Unlock()
		if completed >= target {
			break
		}
		select {
		case <-progress:
		case <-p.done:
			return nil
		case <-ctx.Done():
			p.reporter.Handle(fmt.Errorf("journal logs flush: %w", ctx.Err()))
			return ctx.Err()
		}
	}
	request := journalLogFlush{ctx: ctx, done: make(chan error, 1)}
	select {
	case p.flushes <- request:
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-request.done:
		return err
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *journalLogPipeline) shutdown(ctx context.Context) error {
	p.mu.Lock()
	p.stopping = true
	p.mu.Unlock()
	p.signal()
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		p.cancel()
		// Shutdown runs outside journal locks. Report before returning so a
		// short-lived CLI cannot exit before the worker reports cancellation.
		p.reporter.Handle(fmt.Errorf("journal logs shutdown: %w", ctx.Err()))
		return ctx.Err()
	}
}

type journalLogExporter struct {
	sdklog.Exporter
	pipeline *journalLogPipeline
}

func (e *journalLogExporter) Export(ctx context.Context, records []sdklog.Record) error {
	if err := e.Exporter.Export(ctx, records); err != nil {
		e.pipeline.failures.Add(1)
		e.pipeline.reporter.Handle(fmt.Errorf("journal logs export: %w", err))
		// Already reported with per-client accounting and rate limiting. Do not
		// also pass this to the SDK global handler.
	}
	return nil
}

func newJournalLogExporter(ctx context.Context, cfg Config) (sdklog.Exporter, error) {
	endpoint := strings.TrimSpace(cfg.OTLPEndpoint)
	opts := []otlploggrpc.Option{
		otlploggrpc.WithTimeout(journalLogTimeout),
		// An error may follow collector acceptance. Retrying that record
		// blindly could duplicate it; the durable journal is the source.
		otlploggrpc.WithRetry(otlploggrpc.RetryConfig{Enabled: false}),
	}
	if strings.Contains(endpoint, "://") {
		u, err := url.Parse(endpoint)
		if err != nil || u.Host == "" {
			// WithEndpointURL silently ignores malformed URLs, which would
			// otherwise activate an ambient endpoint or the SDK default.
			return nil, fmt.Errorf("invalid explicit journal logs endpoint %q", endpoint)
		}
		opts = append(opts, otlploggrpc.WithEndpointURL(endpoint))
	} else {
		opts = append(opts, otlploggrpc.WithEndpoint(endpoint))
	}
	if cfg.OTLPInsecure {
		// The Logs SDK prioritizes ambient certificate configuration over
		// WithInsecure. Explicit credentials keep that environment fallback
		// from changing the configured transport.
		opts = append(opts, otlploggrpc.WithInsecure(), otlploggrpc.WithTLSCredentials(insecure.NewCredentials()))
	} else {
		tlsConfig, err := buildOTLPTLSConfig(cfg)
		if err != nil {
			return nil, err
		}
		opts = append(opts, otlploggrpc.WithTLSCredentials(credentials.NewTLS(tlsConfig)))
	}
	headers := make(map[string]string, len(cfg.OTLPHeaders))
	for name, value := range cfg.OTLPHeaders {
		headers[name] = value
	}
	opts = append(opts, otlploggrpc.WithHeaders(headers))
	return otlploggrpc.New(ctx, opts...)
}
