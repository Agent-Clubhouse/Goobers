package telemetry

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	apilog "go.opentelemetry.io/otel/log"
	apimetric "go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/goobers/goobers/internal/journal"
)

const (
	journalLogQueueLimit  = 1024
	journalLogBytesLimit  = 8 << 20
	journalLogRecordLimit = 1 << 20
	journalLogTimeout     = 5 * time.Second
)

// journalDropCause is the closed set of reasons a committed event is not
// exported. It exists so an operator can tell a slow collector from an
// oversized record: those have different fixes, and a single total cannot
// distinguish them (#5573). String values are the metric label values and are
// part of the metric contract, so they change only with the contract.
type journalDropCause int

const (
	dropInvalidMetadata journalDropCause = iota
	dropRecordTooLarge
	dropLockContention
	dropQueueFull
	dropStopping
	dropShutdown
	numJournalDropCauses
)

func (c journalDropCause) String() string {
	switch c {
	case dropInvalidMetadata:
		return "invalid_metadata"
	case dropRecordTooLarge:
		return "record_too_large"
	case dropLockContention:
		return "lock_contention"
	case dropQueueFull:
		return "queue_full"
	case dropStopping:
		return "stopping"
	case dropShutdown:
		return "shutdown"
	}
	return "unknown"
}

// reason is the operator-facing half of a drop message. Each cause keeps one
// stable message signature, because exportErrorHandler rate-limits on the exact
// error string: a message carrying a count would defeat suppression and turn a
// broken collector back into a line-per-drop log storm.
func (c journalDropCause) reason() string {
	switch c {
	case dropInvalidMetadata:
		return "missing persistent journal identity"
	case dropRecordTooLarge:
		return "record exceeds the per-record size limit"
	case dropLockContention:
		return "queue lock was contended and the journal write path cannot wait"
	case dropQueueFull:
		return "queue is full; the collector is not keeping up"
	case dropStopping:
		return "exporter is shutting down"
	case dropShutdown:
		return "shutdown deadline expired with records still queued"
	}
	return "unknown cause"
}

// JournalExportStats is a process-local snapshot. Accepted counts admitted
// records, not collector acknowledgements. Dropped includes overload and records
// abandoned when shutdown expires. ExportFailures counts failed export calls.
type JournalExportStats struct {
	Accepted       uint64
	Dropped        uint64
	ExportFailures uint64
	// SinkPanics is process-wide across all registered journal sinks, not
	// attributed to this client and not included in Dropped.
	SinkPanics uint64
	// InvalidMetadata counts dropped records missing the persistent journal ID.
	// Retained as its own field for compatibility; it equals
	// DroppedByCause["invalid_metadata"].
	InvalidMetadata uint64
	// The per-cause breakdown of Dropped. These sum to Dropped, so "collector
	// too slow" (QueueFull) is distinguishable from "record too big"
	// (RecordTooLarge) and from lock contention, which a single total
	// conflates. Fixed fields rather than a map: a map would make this struct
	// non-comparable and break every consumer comparing snapshots with ==.
	DroppedRecordTooLarge uint64
	DroppedLockContention uint64
	DroppedQueueFull      uint64
	DroppedStopping       uint64
	DroppedShutdown       uint64
	QueuedRecords         int
	QueuedBytes           int
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
	c.journalLogs = newJournalLogPipeline(exporter, res, c.scrubber)
	// Called from the pipeline worker, never from Commit: recording a metric is
	// exporter-adjacent work and the sink contract keeps that off the journal
	// write path.
	c.journalLogs.observeDrops = c.journalExportDropped
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
		InvalidMetadata:       p.invalidMetadata.Load(),
		DroppedRecordTooLarge: p.dropCauses[dropRecordTooLarge].Load(),
		DroppedLockContention: p.dropCauses[dropLockContention].Load(),
		DroppedQueueFull:      p.dropCauses[dropQueueFull].Load(),
		DroppedStopping:       p.dropCauses[dropStopping].Load(),
		DroppedShutdown:       p.dropCauses[dropShutdown].Load(),
		SinkPanics:            journal.CommittedSinkPanicCount(),
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
	scrubber        journal.Scrubber
	// dropCauses breaks dropped down by journalDropCause. Written by drop on the
	// journal write path (atomics only) and published outside that path.
	dropCauses [numJournalDropCauses]atomic.Uint64
	// dropPublishMu lets shutdown settle counts concurrently with the worker
	// without double-publishing a delta. It is never taken by commit/drop.
	dropPublishMu sync.Mutex
	reportedDrops [numJournalDropCauses]uint64
	// observeDrops publishes newly counted drops to the metric. Called only from
	// the worker or shutdown goroutine, never from commit, and nil when the
	// client has no instruments — which includes JournalLogsOnly mode, where the
	// stats API is the only channel.
	observeDrops func(cause journalDropCause, delta uint64)
}

func newJournalLogPipeline(exporter sdklog.Exporter, res *resource.Resource, scrubber journal.Scrubber) *journalLogPipeline {
	ctx, cancel := context.WithCancel(context.Background())
	p := &journalLogPipeline{
		scrubber: scrubber,
		ctx:      ctx, cancel: cancel, progress: make(chan struct{}),
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

// drop records one unexported event and wakes the worker. It runs on the
// journal's write path, so it does only atomic adds: publishing the metric and
// logging the cause belong to the worker, because the CommittedEventSink
// contract forbids I/O and exporter callbacks under the journal write lock.
func (p *journalLogPipeline) drop(cause journalDropCause) {
	p.dropCauses[cause].Add(1)
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
		p.drop(dropInvalidMetadata)
		return
	}
	size := journalLogSize(e)
	if size > journalLogRecordLimit {
		p.drop(dropRecordTooLarge)
		return
	}
	// Copy before taking the lock, not under it (#5575). The copy itself has to
	// stay: TestJournalLogsQueueBoundsAndOwnership pins that a caller mutating
	// its slice after Commit cannot alter an already-queued record, which is a
	// defense the ownership contract asks for but cannot enforce. Doing it here
	// keeps a memcpy of up to journalLogRecordLimit bytes out of the journal
	// write lock, and out of the TryLock window whose loss is counted below as
	// dropLockContention.
	e.Body = append([]byte(nil), e.Body...)
	e.Kind = strings.Clone(e.Kind)
	e.JournalID = strings.Clone(e.JournalID)
	e.InstanceID = strings.Clone(e.InstanceID)
	e.Gaggle = strings.Clone(e.Gaggle)
	e.RunID = strings.Clone(e.RunID)
	if !p.mu.TryLock() {
		// Distinct from queue_full: the queue may be empty and the collector
		// healthy. This is contention on the queue lock itself, which the
		// journal write path must never wait on.
		p.drop(dropLockContention)
		return
	}
	if p.stopping {
		p.mu.Unlock()
		p.drop(dropStopping)
		return
	}
	if p.pending >= journalLogQueueLimit || size > journalLogBytesLimit-p.bytes {
		p.mu.Unlock()
		p.drop(dropQueueFull)
		return
	}
	p.queue[(p.head+p.length)%len(p.queue)] = journalLogItem{event: e, bytes: size}
	p.length++
	p.pending++
	p.bytes += size
	p.accepted.Add(1)
	p.mu.Unlock()
	p.signal()
}

// journalExportDropped records delta unexported committed events attributed to
// cause. A nil instruments set (any client built without a metric reader,
// including JournalLogsOnly mode) makes this a no-op and leaves
// JournalExportStats as the only channel.
func (c *Client) journalExportDropped(cause journalDropCause, delta uint64) {
	if c == nil || c.instruments == nil || delta == 0 {
		return
	}
	c.instruments.journalExportDrops.Add(context.Background(), int64(delta),
		apimetric.WithAttributes(attribute.String(MetricAttrJournalDropCause, cause.String())))
}

// publishDrops emits one log line and one metric increment per cause that has
// gained drops since the last call. Runs on the worker goroutine only.
//
// Each cause keeps its own stable message signature, so exportErrorHandler
// suppresses a recurring cause on its own 30-minute window instead of one shared
// window hiding a second, different cause behind the first. Six causes sit far
// inside the handler's 64-signature table.

func (p *journalLogPipeline) publishDrops() {
	p.dropPublishMu.Lock()
	defer p.dropPublishMu.Unlock()
	for cause := journalDropCause(0); cause < numJournalDropCauses; cause++ {
		total := p.dropCauses[cause].Load()
		delta := total - p.reportedDrops[cause]
		if delta == 0 {
			continue
		}
		p.reportedDrops[cause] = total
		if p.observeDrops != nil {
			p.observeDrops(cause, delta)
		}
		p.reporter.Handle(fmt.Errorf("journal logs dropped (%s): %s; inspect JournalExportStats",
			cause, cause.reason()))
	}
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
	started := false
	for {
		p.publishDrops()
		select {
		case request := <-p.flushes:
			err := p.provider.ForceFlush(request.ctx)
			if err != nil {
				p.reporter.Handle(fmt.Errorf("journal logs flush: %w", err))
			}
			request.done <- err
		default:
		}
		p.publishDrops()
		p.mu.Lock()
		if p.ctx.Err() != nil {
			abandoned := uint64(p.length)
			p.dropCauses[dropShutdown].Add(abandoned)
			p.dropped.Add(abandoned)
			clear(p.queue[:])
			p.length, p.pending, p.bytes = 0, 0, 0
			close(p.progress)
			p.progress = make(chan struct{})
			p.mu.Unlock()
			p.publishDrops()
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
	// Gaggle is the only operator-supplied free text among the identity
	// attributes, so it is the only one that can plausibly carry a pasted
	// credential; the body it travels with was already scrubbed by the journal.
	// The remaining attributes are machine-generated identifiers and are
	// deliberately left raw: they are the correlation keys, and running a
	// secret-shaped pattern net over an opaque hex id risks redacting the very
	// values a consumer joins on. Keep them out of the scrubber.
	gaggle := e.Gaggle
	if gaggle != "" && p.scrubber != nil {
		gaggle = redactWith(p.scrubber, gaggle)
	}
	for _, kv := range []attribute.KeyValue{
		attribute.String("goobers.instance.id", e.InstanceID),
		attribute.String("goobers.gaggle", gaggle),
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
		// A commit that observed stopping can race the worker's final loop.
		// Settle that last delta before Client shuts down the metric provider.
		p.publishDrops()
		return nil
	case <-ctx.Done():
		p.cancel()
		// Account the abandoned backlog before returning. The worker performs
		// the same accounting when it observes the cancelled context, but a
		// short-lived CLI reads JournalExportStats as soon as Shutdown returns
		// and would otherwise report Dropped: 0 for records that were never
		// sent. Only the queued records are settled here: pending and bytes
		// still cover a record the worker may be mid-emit on, and the worker
		// resets both once it exits. Both paths run under p.mu, so whichever
		// arrives second adds zero.
		p.mu.Lock()
		abandoned := uint64(p.length)
		p.dropCauses[dropShutdown].Add(abandoned)
		p.dropped.Add(abandoned)
		clear(p.queue[:])
		p.length = 0
		p.mu.Unlock()
		// The worker may still be blocked in the exporter. Publish the abandoned
		// backlog synchronously so Client can export its metric before returning.
		p.publishDrops()
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
