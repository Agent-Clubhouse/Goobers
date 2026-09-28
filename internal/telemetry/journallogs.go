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
	journalLogBatchLimit  = 128
	journalLogBatchBytes  = 256 << 10 // Charged input bytes, not encoded wire bytes.
	journalLogBatchDelay  = 100 * time.Millisecond
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
	AzureReplay           AzureReplayStats
	// CatchupDeferred counts dropped wake hints, not lost journal records.
	// Background discovery recovers those records from the authoritative journal.
	CatchupDeferred uint64
}

var _ journal.CommittedEventSink = (*Client)(nil)

func (c *Client) configureJournalLogs(ctx context.Context, cfg Config, res *resource.Resource) error {
	if !cfg.JournalLogs && !cfg.AzureMonitorJournalLogs {
		return nil
	}
	exporters := make([]sdklog.Exporter, 0, 2)
	durableJournal := false
	var degraded error
	if cfg.Exporter == ExporterOTLP && strings.TrimSpace(cfg.OTLPEndpoint) != "" {
		exporter, err := newJournalLogExporter(ctx, cfg)
		if err != nil {
			degraded = fmt.Errorf("%w: create journal logs exporter: %w", ErrOTLPUnavailable, err)
		} else {
			exporters = append(exporters, exporter)
		}
	}
	if cfg.AzureMonitorConnectionString != "" && cfg.AzureMonitorJournalLogs {
		exporter, err := newAzureMonitorLogExporter(cfg.AzureMonitorConnectionString, cfg.AzureMonitorHTTPClient, cfg.AzureMonitorHostIdentity, cfg.azureReplayConfig("journal"))
		if err != nil {
			if len(exporters) == 0 {
				return err
			}
			degraded = errors.Join(degraded, err)
		} else {
			exporters = append(exporters, exporter)
			durableJournal = cfg.AzureMonitorReplayRoot != ""
		}
	}
	if len(exporters) == 0 {
		return degraded
	}
	exporter := exporters[0]
	if len(exporters) > 1 {
		exporter = journalLogFanoutExporter(exporters)
	}
	c.journalLogs = newJournalLogPipeline(exporter, res, c.scrubber)
	// Called from the pipeline worker, never from Commit: recording a metric is
	// exporter-adjacent work and the sink contract keeps that off the journal
	// write path.
	c.journalLogs.observeDrops = c.journalExportDropped
	if cfg.JournalRoot == "" {
		return nil
	}
	if durableJournal {
		c.journalCatchup = newJournalCatchup(cfg, c.journalLogs)
	}
	unregister, err := journal.RegisterCommittedEventSink(cfg.JournalRoot, cfg.JournalInstanceID, c)
	if err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), journalLogTimeout)
		defer cancel()
		if c.journalCatchup != nil {
			c.journalCatchup.shutdown(shutdownCtx)
		}
		_ = c.journalLogs.shutdown(shutdownCtx)
		c.journalLogs = nil
		return fmt.Errorf("%w: register journal logs: %w", ErrOTLPUnavailable, err)
	}
	c.unregisterJournal = unregister
	return degraded
}

type journalLogFanoutExporter []sdklog.Exporter

func (e journalLogFanoutExporter) setReplayLossSource(sample func() replayLossCounters) {
	for _, exporter := range e {
		if target, ok := exporter.(interface {
			setReplayLossSource(func() replayLossCounters)
		}); ok {
			target.setReplayLossSource(sample)
		}
	}
}

func (e journalLogFanoutExporter) ReplayStats() AzureReplayStats {
	for _, exporter := range e {
		if source, ok := exporter.(interface{ ReplayStats() AzureReplayStats }); ok {
			return source.ReplayStats()
		}
	}
	return AzureReplayStats{}
}

func (e journalLogFanoutExporter) Export(ctx context.Context, records []sdklog.Record) error {
	var errs []error
	for _, exporter := range e {
		errs = append(errs, exporter.Export(ctx, records))
	}
	return errors.Join(errs...)
}

func (e journalLogFanoutExporter) ForceFlush(ctx context.Context) error {
	var errs []error
	for _, exporter := range e {
		errs = append(errs, exporter.ForceFlush(ctx))
	}
	return errors.Join(errs...)
}

func (e journalLogFanoutExporter) Shutdown(ctx context.Context) error {
	var errs []error
	for _, exporter := range e {
		errs = append(errs, exporter.Shutdown(ctx))
	}
	return errors.Join(errs...)
}

// Commit only copies into a bounded queue. It never exports or logs while the
// caller holds a journal lock. The journal remains authoritative on overload.
func (c *Client) Commit(event journal.CommittedEvent) {
	if c != nil && c.journalLogs != nil {
		if c.journalCatchup != nil {
			c.journalCatchup.notify(event)
			return
		}
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
	queuedRecords, queuedBytes := p.reserved()
	stats := JournalExportStats{
		Accepted: p.accepted.Load(), Dropped: p.dropped.Load(),
		ExportFailures: p.failures.Load(), QueuedRecords: queuedRecords, QueuedBytes: queuedBytes,
		InvalidMetadata:       p.invalidMetadata.Load(),
		DroppedRecordTooLarge: p.dropCauses[dropRecordTooLarge].Load(),
		DroppedLockContention: p.dropCauses[dropLockContention].Load(),
		DroppedQueueFull:      p.dropCauses[dropQueueFull].Load(),
		DroppedStopping:       p.dropCauses[dropStopping].Load(),
		DroppedShutdown:       p.dropCauses[dropShutdown].Load(),
		SinkPanics:            journal.CommittedSinkPanicCount(),
		CatchupDeferred:       p.catchupDeferred.Load(),
	}
	p.mu.Unlock()
	// Replay inspection can touch disk; keep it outside the worker state lock.
	stats.AzureReplay = p.azureReplayStats()
	return stats
}

func (p *journalLogPipeline) azureReplayStats() AzureReplayStats {
	if p == nil || p.replayStats == nil {
		return AzureReplayStats{}
	}
	return p.replayStats()
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
	incoming        chan journalLogItem
	admission       atomic.Uint64 // stopping bit | reserved count | reserved bytes
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
	batch           *journalBatchProcessor
	reporter        *exportErrorHandler
	accepted        atomic.Uint64
	dropped         atomic.Uint64
	failures        atomic.Uint64
	invalidMetadata atomic.Uint64
	completed       atomic.Uint64
	catchupDeferred atomic.Uint64
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
	replayStats  func() AzureReplayStats
}

func newJournalLogPipeline(exporter sdklog.Exporter, res *resource.Resource, scrubber journal.Scrubber) *journalLogPipeline {
	ctx, cancel := context.WithCancel(context.Background())
	p := &journalLogPipeline{
		scrubber: scrubber,
		ctx:      ctx, cancel: cancel, progress: make(chan struct{}),
		wake: make(chan struct{}, 1), done: make(chan struct{}), ready: make(chan struct{}),
		flushes:  make(chan journalLogFlush, 1),
		incoming: make(chan journalLogItem, journalLogQueueLimit),
		reporter: newExportErrorHandler(),
	}
	if source, ok := exporter.(interface{ ReplayStats() AzureReplayStats }); ok {
		p.replayStats = source.ReplayStats
	}
	if target, ok := exporter.(interface {
		setReplayLossSource(func() replayLossCounters)
	}); ok {
		target.setReplayLossSource(func() replayLossCounters {
			return replayLossCounters{Dropped: p.dropped.Load(), ExportFailures: p.failures.Load(), CatchupDeferred: p.catchupDeferred.Load()}
		})
	}
	// The application queue is the only lossy boundary. An SDK asynchronous
	// batch processor would add a queue whose losses could not be accounted for.
	// Instead the worker gathers one bounded batch; this processor only keeps
	// the SDK-enriched records for that batch, with no second queue or worker.
	p.batch = &journalBatchProcessor{exporter: &journalLogExporter{Exporter: exporter, pipeline: p}}
	p.provider = sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		// Fixed correlation fields now include workflow/digest/stage/attempt in
		// addition to the journal envelope. Keep modest headroom for versioned
		// additions without silently dropping the last correlation key.
		sdklog.WithAttributeCountLimit(16),
		sdklog.WithAttributeValueLengthLimit(-1),
		sdklog.WithProcessor(p.batch),
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
	return len(e.Body) + len(e.Kind) + len(e.JournalID) + len(e.InstanceID) + len(e.Gaggle) +
		len(e.Workflow) + len(e.WorkflowDigest) + len(e.ConfigGeneration) + len(e.TriggerKind) + len(e.RunID) + len(e.Stage)
}

const journalAdmissionStopping uint64 = 1 << 63

func (p *journalLogPipeline) reserved() (int, int) {
	state := p.admission.Load()
	return int((state &^ journalAdmissionStopping) >> 32), int(uint32(state))
}

// Reserve both limits before copying. CAS contention retries instead of losing
// records; no consumer mutex, disk work or exporter callback is on this path.
// Reservations include copies in progress, queued items and in-flight batches.
func (p *journalLogPipeline) reserve(size int) (journalDropCause, bool) {
	for {
		state := p.admission.Load()
		if state&journalAdmissionStopping != 0 {
			return dropStopping, false
		}
		if state>>32 >= journalLogQueueLimit || size > journalLogBytesLimit-int(uint32(state)) {
			return dropQueueFull, false
		}
		if p.admission.CompareAndSwap(state, state+(1<<32)+uint64(size)) {
			return 0, true
		}
	}
}

func (p *journalLogPipeline) release(count, size int) {
	p.admission.Add(^(uint64(count)<<32 + uint64(size)) + 1)
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
	if cause, ok := p.reserve(size); !ok {
		p.drop(cause)
		return
	}
	// Keep ownership independent of caller buffers after Commit returns.
	e.Body = append([]byte(nil), e.Body...)
	e.Kind = strings.Clone(e.Kind)
	e.JournalID = strings.Clone(e.JournalID)
	e.InstanceID = strings.Clone(e.InstanceID)
	e.Gaggle = strings.Clone(e.Gaggle)
	e.Workflow = strings.Clone(e.Workflow)
	e.WorkflowDigest = strings.Clone(e.WorkflowDigest)
	e.ConfigGeneration = strings.Clone(e.ConfigGeneration)
	e.TriggerKind = strings.Clone(e.TriggerKind)
	e.RunID = strings.Clone(e.RunID)
	e.Stage = strings.Clone(e.Stage)
	p.accepted.Add(1)
	// Each sender owns a reservation and the channel has the full count limit,
	// so an admitted send always fits, even when the consumer is paused.
	p.incoming <- journalLogItem{event: e, bytes: size}
	p.signal()
}

// collectLocked transfers published reservations into the worker's batch ring.
func (p *journalLogPipeline) collectLocked() {
	for {
		select {
		case item := <-p.incoming:
			p.queue[(p.head+p.length)%len(p.queue)] = item
			p.length++
		default:
			return
		}
	}
}

func (p *journalLogPipeline) abandonLocked() {
	p.collectLocked()
	for p.length > 0 {
		item := p.queue[p.head]
		p.queue[p.head] = journalLogItem{}
		p.head = (p.head + 1) % len(p.queue)
		p.length--
		p.release(1, item.bytes)
		p.drop(dropShutdown)
	}
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
		p.collectLocked()
		if p.ctx.Err() != nil {
			p.abandonLocked()
			pending, _ := p.reserved()
			close(p.progress)
			p.progress = make(chan struct{})
			p.mu.Unlock()
			p.publishDrops()
			if pending > 0 {
				<-p.wake // A producer reserved before shutdown and is still copying.
				continue
			}
			return
		}
		if p.length == 0 {
			pending, _ := p.reserved()
			stopping := p.stopping && pending == 0
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
				// Coalesce a newly arriving burst. This is a maximum batching
				// delay, never a delay between batches of an existing backlog.
				p.coalesce(journalLogBatchDelay)
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
		var batch [journalLogBatchLimit]journalLogItem
		count, size := 0, 0
		for p.length > 0 && count < len(batch) {
			item := p.queue[p.head]
			// An individually valid large record travels alone.
			if count > 0 && size+item.bytes > journalLogBatchBytes {
				break
			}
			batch[count] = item
			count++
			size += item.bytes
			p.queue[p.head] = journalLogItem{}
			p.head = (p.head + 1) % len(p.queue)
			p.length--
		}
		p.mu.Unlock()

		for i := range count {
			p.emit(batch[i].event)
		}
		ctx, cancel := context.WithTimeout(p.ctx, journalLogTimeout)
		_ = p.batch.ForceFlush(ctx) // journalLogExporter accounts export errors.
		cancel()

		p.mu.Lock()
		p.release(count, size)
		p.completed.Add(uint64(count))
		close(p.progress)
		p.progress = make(chan struct{})
		p.mu.Unlock()
	}
}

func (p *journalLogPipeline) coalesce(delay time.Duration) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		// There is nothing left to coalesce once a bounded batch is full.
		// In particular, durable catch-up admits one read batch then waits
		// for its acknowledgement; imposing the idle delay on each full
		// byte-limited batch artificially throttles large-record recovery.
		count, size := p.reserved()
		if count >= journalLogBatchLimit || size >= journalLogBatchBytes {
			return
		}
		select {
		case <-timer.C:
			return
		case <-p.ctx.Done():
			return
		case <-p.wake:
			// Existing producer notifications announce that the batch grew.
			// Do not restart the timer: sparse traffic still waits at most delay.
		}
	}
}

func (p *journalLogPipeline) emit(e journal.CommittedEvent) {
	var record apilog.Record
	record.SetTimestamp(e.Time)
	record.SetObservedTimestamp(e.ObservedTime)
	record.SetBody(attribute.StringValue(string(e.Body)))
	record.AddAttributes(
		attribute.Int("goobers.journal.schema_version", 1),
		attribute.String("goobers.telemetry.stream", "journal"),
		attribute.String("goobers.journal.kind", e.Kind),
		attribute.String("goobers.journal.id", e.JournalID),
		attribute.String("goobers.journal.seq", strconv.FormatUint(e.Seq, 10)),
	)
	// Human/config-authored labels use the same scrubber as the body. Opaque
	// machine IDs and digests remain raw: a pattern net can mistake their random
	// bytes for a credential and destroy the correlation key.
	scrubLabel := func(value string) string {
		if value != "" && p.scrubber != nil {
			return redactWith(p.scrubber, value)
		}
		return value
	}
	for _, kv := range []attribute.KeyValue{
		attribute.String("goobers.instance.id", e.InstanceID),
		attribute.String("goobers.gaggle", scrubLabel(e.Gaggle)),
		attribute.String(AttrWorkflow, scrubLabel(e.Workflow)),
		attribute.String(AttrWorkflowDigest, e.WorkflowDigest),
		attribute.String(AttrConfigGeneration, scrubLabel(e.ConfigGeneration)),
		attribute.String(AttrTriggerKind, e.TriggerKind),
		attribute.String("goobers.run.id", e.RunID),
		attribute.String(AttrStage, scrubLabel(e.Stage)),
	} {
		if kv.Value.AsString() != "" {
			record.AddAttributes(kv)
		}
	}
	if e.WorkflowVersion > 0 {
		record.AddAttributes(attribute.Int(AttrWorkflowVersion, e.WorkflowVersion))
	}
	if e.Attempt > 0 {
		record.AddAttributes(attribute.Int(AttrAttemptNumber, e.Attempt))
	}
	ctx := p.ctx
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
	p.admission.Or(journalAdmissionStopping)
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
		p.abandonLocked()
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

// All calls originate on the pipeline worker. The mutex also satisfies the
// SDK processor concurrency contract without moving I/O onto Commit.
type journalBatchProcessor struct {
	mu       sync.Mutex
	exporter sdklog.Exporter
	records  []sdklog.Record
	closed   bool
}

func (*journalBatchProcessor) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }

func (b *journalBatchProcessor) OnEmit(_ context.Context, record *sdklog.Record) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.closed {
		b.records = append(b.records, record.Clone())
	}
	return nil
}

func (b *journalBatchProcessor) ForceFlush(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	return b.flushLocked(ctx)
}

func (b *journalBatchProcessor) flushLocked(ctx context.Context) error {
	if len(b.records) > 0 {
		err := b.exporter.Export(ctx, b.records)
		clear(b.records)
		b.records = b.records[:0]
		if err != nil {
			return err
		}
	}
	return b.exporter.ForceFlush(ctx)
}

func (b *journalBatchProcessor) Shutdown(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	return errors.Join(b.flushLocked(ctx), b.exporter.Shutdown(ctx))
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
