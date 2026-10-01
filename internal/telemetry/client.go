// Package telemetry exposes the small span-helper API used by the scheduler,
// workflow engine, and goober runtime. Callers describe Goobers domain events;
// this package owns the OpenTelemetry setup and attribute mapping.
package telemetry

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	apimetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc/credentials"

	"github.com/goobers/goobers/internal/journal"
)

const (
	// ScopeName is the instrumentation scope name used for Goobers telemetry.
	ScopeName = "github.com/goobers/goobers/internal/telemetry"

	// ExporterOTLP sends spans to an OTLP gRPC collector.
	ExporterOTLP ExporterKind = "otlp"
	// ExporterStdout writes spans to stdout for local development.
	ExporterStdout ExporterKind = "stdout"
)

// ExporterKind selects the built-in span exporter.
type ExporterKind string

// Config controls tracer/meter setup and exporter selection. ExporterOTLP
// requires a non-empty, explicitly configured OTLPEndpoint.
type Config struct {
	ServiceName    string
	ServiceVersion string
	BuildCommit    string
	Environment    string
	Exporter       ExporterKind
	OTLPEndpoint   string
	OTLPInsecure   bool
	OTLPHeaders    map[string]string
	// AzureMonitorConnectionString enables direct, customer-owned Application
	// Insights trace export alongside local journal and optional OTLP export.
	AzureMonitorConnectionString string
	// AzureMonitorHTTPClient is injectable for transport tests. Production
	// callers leave it nil and use the standard HTTP client.
	AzureMonitorHTTPClient *http.Client
	// AzureMonitorTraces and AzureMonitorJournalLogs independently select the
	// direct destination's signal set. They do not alter explicit OTLP routing.
	AzureMonitorTraces      bool
	AzureMonitorJournalLogs bool
	// AzureMonitorHostIdentity permits hostname context tags. It must only be
	// set by an explicit diagnostic collection profile.
	AzureMonitorHostIdentity bool
	// AzureMonitorReplayRoot enables the bounded disk-backed replay spool.
	// Age and bytes must be positive when the root is set.
	AzureMonitorReplayRoot     string
	AzureMonitorReplayMaxAge   time.Duration
	AzureMonitorReplayMaxBytes int64
	// AzureMonitorReplayStart, when non-nil, delays only background replay
	// until the caller closes the channel (for example, after daemon readiness).
	// Accounting, durable admission, and bounded shutdown remain active. Each
	// caller owns its signal; closing an exporter never releases another one.
	// Nil preserves immediate replay for one-shot commands and other callers.
	AzureMonitorReplayStart <-chan struct{}
	// JournalLogs enables live export of committed journal events as OTLP Logs.
	// It has no effect without ExporterOTLP and an explicit endpoint.
	JournalLogs bool
	// JournalLogsOnly omits trace and metric providers for short-lived
	// commands. JournalLogs and the explicit OTLP endpoint still gate export.
	JournalLogsOnly bool
	// JournalRoot optionally attaches live export to this existing instance
	// root. The client releases the registration before shutdown.
	JournalRoot string
	// JournalInstanceID is the known instance identity attached to committed
	// events. An empty value omits the instance attribute.
	JournalInstanceID string
	// OTLPCAFile is an extra PEM root appended to the system trust pool
	// (RootCAs). Empty means system trust only, the pre-#3804 behavior.
	OTLPCAFile string
	// OTLPServerName overrides SNI/verification. Empty uses the endpoint's
	// own host.
	OTLPServerName string
	// OTLPCertFile and OTLPKeyFile present a client certificate for mTLS.
	// Both or neither — instance.OTLPTLSConfig.Validate enforces the pairing
	// before this Config is ever built.
	OTLPCertFile string
	OTLPKeyFile  string
	Stdout       io.Writer
	SpanExporter sdktrace.SpanExporter
	// MetricReader collects metrics directly from the meter provider,
	// alongside any configured exporter. Tests inject a manual reader.
	MetricReader metric.Reader
	// MetricExporter is attached to the meter provider behind a periodic
	// reader, exactly like the OTLP metric exporter.
	MetricExporter metric.Exporter
	// ExporterHealth receives bounded local observations for remote telemetry
	// exporter health. Nil disables exporter-health accounting.
	ExporterHealth *ExporterHealth
	// MetricExportInterval overrides the periodic reader's export period.
	// Zero uses metricExportInterval.
	MetricExportInterval time.Duration
	Scrubber             journal.Scrubber
	ResourceAttributes   []attribute.KeyValue
	Batch                bool
}

// ErrOTLPUnavailable is the sentinel New wraps into its returned error when
// the configured OTLP exporter's TLS material could not be loaded (an
// unreadable CA file, or an unparsable client certificate/key pair). Unlike
// an unreachable endpoint — otlptracegrpc dials lazily, so a bad address
// never fails New — a bad TLS path is detectable right now, at
// construction, so it would otherwise introduce a NEW boot-fatal class: a
// CA path typo becoming a daemon outage (the same shape #3804 exists to
// avoid, ledger L-28).
//
// When New's error wraps ErrOTLPUnavailable, the returned *Client is still
// valid: every OTHER exporter (notably SpanExporter, the local journal
// export every process wires) is fully constructed and usable. A caller
// that wants the process to stay up should check errors.Is(err,
// ErrOTLPUnavailable), log the cause loudly (the daemon's convention is
// instance-journal code telemetry_otlp_unavailable), and keep the returned
// client rather than discarding it as a construction failure.
var ErrOTLPUnavailable = errors.New("otlp exporter unavailable")

// Client owns the OTel tracer and meter providers for a Goobers process.
type Client struct {
	tracerProvider     *sdktrace.TracerProvider
	localSpanProcessor sdktrace.SpanProcessor
	meterProvider      *metric.MeterProvider
	instruments        *instruments
	tracer             trace.Tracer
	scrubber           journal.Scrubber
	journalLogs        *journalLogPipeline
	journalCatchup     *journalCatchup
	unregisterJournal  func()
	exporterHealth     *ExporterHealth
}

// InstanceJournalAppendDropped implements journal.InstanceAppendDropObserver.
// The metric is deliberately dimensionless: paths, errors, run IDs, and event
// types are either sensitive or unbounded and do not belong in metric labels.
func (c *Client) InstanceJournalAppendDropped() {
	if c == nil || c.instruments == nil {
		return
	}
	c.instruments.journalDrops.Add(context.Background(), 1)
}

// SnapshotCaptured implements internal/recovery's SnapshotObserver (that
// package sits downstream of this one in the import graph, so the interface
// it declares uses plain strings rather than a type imported from here).
// bytes and format are bounded/verified by the recovery package before this
// is called: format is one of two literal strings, and bytes is the
// archive's own byte budget.
func (c *Client) SnapshotCaptured(format string, bytes int64) {
	if c == nil || c.instruments == nil {
		return
	}
	attrs := c.instruments.attributeSet(attribute.String(MetricAttrRecoverySnapshotFormat, format))
	ctx := context.Background()
	c.instruments.recoverySnapshotFormat.Add(ctx, 1, attrs)
	c.instruments.recoverySnapshotBytes.Record(ctx, float64(bytes), attrs)
}

// SnapshotFallback implements internal/recovery's SnapshotObserver. reason is
// one of that package's FallbackReason constants.
func (c *Client) SnapshotFallback(reason string) {
	if c == nil || c.instruments == nil {
		return
	}
	c.instruments.recoverySnapshotFallback.Add(context.Background(), 1,
		c.instruments.attributeSet(attribute.String(MetricAttrRecoveryReason, reason)))
}

// SnapshotRestoreFailed implements internal/recovery's SnapshotObserver.
// reason is one of that package's RestoreFailureReason constants.
func (c *Client) SnapshotRestoreFailed(reason string) {
	if c == nil || c.instruments == nil {
		return
	}
	c.instruments.recoveryRestoreFailures.Add(context.Background(), 1,
		c.instruments.attributeSet(attribute.String(MetricAttrRecoveryReason, reason)))
}

// StorageHealthSampled records one tiered low-disk protection reading
// (#4873): the current free-byte gauge always updates, and tierChanged is
// true only on the sample where the tier actually differs from the previous
// one — the counter increments once per transition, not once per sample, so
// a quiet instance sitting in one tier does not manufacture the appearance of
// repeated events.
func (c *Client) StorageHealthSampled(tier string, freeBytes uint64, tierChanged bool) {
	if c == nil || c.instruments == nil {
		return
	}
	ctx := context.Background()
	c.instruments.storageFreeBytes.Record(ctx, int64(freeBytes))
	if tierChanged {
		c.instruments.storageHealthChanges.Add(ctx, 1, apimetric.WithAttributes(attribute.String(MetricAttrStorageTier, tier)))
	}
}

// QueueSaturationSample is one scheduler-visible queue lane snapshot. QueueKind
// must be a closed, bounded value such as "schedule", "backlog", or "refill".
// OldestEnqueuedAt is the canonical enqueue/readiness time for the oldest item
// represented by Depth; zero means no age source is available. Empty queues
// still record age 0, but non-empty queues without a source omit age instead
// of guessing.
type QueueSaturationSample struct {
	QueueKind        string
	OperatingSystem  string
	Depth            int
	OldestEnqueuedAt time.Time
	ObservedAt       time.Time
}

// WorkerAvailabilitySample is a scheduler capacity snapshot. Emit it only when
// the scheduler has a reliable configured-capacity source; zero is a valid value.
type WorkerAvailabilitySample struct {
	OperatingSystem string
	Available       int
}

// RecordSchedulerQueueSaturation publishes queue depth, oldest queue age, and
// worker availability gauges. Nil clients and telemetry-disabled clients are
// no-ops, preserving optional, non-blocking export semantics.
func (c *Client) RecordSchedulerQueueSaturation(ctx context.Context, queues []QueueSaturationSample, workers *WorkerAvailabilitySample) {
	if c == nil || c.instruments == nil {
		return
	}
	for _, queue := range queues {
		depth := queue.Depth
		if depth < 0 {
			depth = 0
		}
		attrs := queueSaturationAttrs(queue.QueueKind, queue.OperatingSystem)
		c.instruments.queueDepth.Record(ctx, int64(depth), c.instruments.attributeSet(attrs...))
		if depth > 0 && queue.OldestEnqueuedAt.IsZero() {
			continue
		}
		ageSeconds := int64(0)
		if depth > 0 {
			observedAt := queue.ObservedAt
			if observedAt.IsZero() {
				observedAt = time.Now()
			}
			if age := observedAt.Sub(queue.OldestEnqueuedAt); age > 0 {
				ageSeconds = int64(age.Seconds())
			}
		}
		c.instruments.queueOldestAge.Record(ctx, ageSeconds, c.instruments.attributeSet(attrs...))
	}
	if workers == nil {
		return
	}
	available := workers.Available
	if available < 0 {
		available = 0
	}
	c.instruments.workersAvailable.Record(ctx, int64(available),
		c.instruments.attributeSet(queueSaturationAttrs("", workers.OperatingSystem)...))
}

func queueSaturationAttrs(queueKind, operatingSystem string) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 2)
	if queueKind != "" {
		attrs = append(attrs, attribute.String(MetricAttrQueueKind, queueKind))
	}
	if operatingSystem != "" {
		attrs = append(attrs, attribute.String(MetricAttrOS, operatingSystem))
	}
	return attrs
}

// New configures OpenTelemetry tracing and metrics for a Goobers process.
func New(ctx context.Context, cfg Config) (*Client, error) {
	// The SDK's default error handler logs every asynchronous export failure
	// unconditionally; in a daemon that is one line a minute for as long as the
	// condition lasts (#4159). Replace it before any exporter can report.
	installExportErrorHandler()

	scrubber := cfg.Scrubber
	if scrubber == nil {
		// Do not attach client-local metric observation to providerNet: that
		// scrubber is the package-global net used by Redact, so a second Client
		// would replace the first one's observer and unrelated Redact calls would
		// be charged to whichever client was constructed last.
		scrubber = journal.NewPatternScrubber()
	}

	instanceID, err := NewRunID()
	if err != nil {
		return nil, fmt.Errorf("generate telemetry service instance id: %w", err)
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(
			attribute.String("service.name", "goobers"),
			attribute.String("service.instance.id", instanceID),
		),
		resource.WithAttributes(resourceAttrs(cfg)...),
		resource.WithFromEnv(),
	)
	if err != nil {
		return nil, fmt.Errorf("build telemetry resource: %w", err)
	}
	res = resource.NewWithAttributes(res.SchemaURL(), scrubAttributes(scrubber, res.Attributes())...)
	if cfg.JournalLogsOnly {
		client := &Client{
			scrubber:       scrubber,
			tracer:         tracenoop.NewTracerProvider().Tracer(ScopeName),
			exporterHealth: cfg.ExporterHealth,
		}
		return client, client.configureJournalLogs(ctx, cfg, res)
	}

	// otlpDegraded, not err, carries an ErrOTLPUnavailable across the rest of
	// this function: err gets reused (:=) by later steps below, and the
	// degrade signal must survive to the final return regardless.
	exporters, err := spanExporters(ctx, cfg)
	var otlpDegraded error
	if err != nil {
		if !errors.Is(err, ErrOTLPUnavailable) {
			return nil, err
		}
		otlpDegraded = err
	}

	spanLimits := sdktrace.NewSpanLimits()
	// Bound within-attempt event accumulation. The SDK reports overflow through
	// ReadOnlySpan.DroppedEvents.
	spanLimits.EventCountLimit = maxSpanEvents
	// Ingest rejects larger attribute maps and reserves room for required
	// telemetry metadata, so accepted records cannot be partially truncated.
	spanLimits.AttributePerEventCountLimit = maxSpanEventAttributes
	options := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithIDGenerator(runIDGenerator{}),
		sdktrace.WithRawSpanLimits(spanLimits),
	}
	processors := make([]sdktrace.SpanProcessor, 0, len(exporters))
	for _, exporter := range exporters {
		var processor sdktrace.SpanProcessor
		if cfg.Batch {
			processor = sdktrace.NewBatchSpanProcessor(exporter)
		} else {
			processor = sdktrace.NewSimpleSpanProcessor(exporter)
		}
		processors = append(processors, processor)
		options = append(options, sdktrace.WithSpanProcessor(processor))
	}

	tracerProvider := sdktrace.NewTracerProvider(options...)

	readers, err := metricReaders(ctx, cfg)
	if err != nil {
		if !errors.Is(err, ErrOTLPUnavailable) {
			return nil, err
		}
		if otlpDegraded == nil {
			otlpDegraded = err
		}
	}
	meterOptions := make([]metric.Option, 0, len(readers)+1)
	meterOptions = append(meterOptions, metric.WithResource(res))
	for _, reader := range readers {
		meterOptions = append(meterOptions, metric.WithReader(reader))
	}
	meterProvider := metric.NewMeterProvider(meterOptions...)

	client := &Client{
		tracerProvider: tracerProvider,
		meterProvider:  meterProvider,
		tracer:         tracerProvider.Tracer(ScopeName),
		scrubber:       scrubber,
		exporterHealth: cfg.ExporterHealth,
	}
	if len(readers) != 0 {
		// A meter provider with no reader records nothing, so instruments are
		// built only when something collects them. An instrument-construction
		// failure disables metrics rather than failing telemetry setup: metric
		// export is optional and must never become a new boot-fatal class.
		if inst, instErr := newInstruments(meterProvider.Meter(ScopeName)); instErr == nil {
			client.instruments = inst
			journal.ObserveRedactions(scrubber, redactionMetricObserver{counter: inst.redactions})
		}
	}
	if cfg.SpanExporter != nil {
		client.localSpanProcessor = processors[0]
	}
	otlpDegraded = errors.Join(otlpDegraded, client.configureJournalLogs(ctx, cfg, res))
	return client, otlpDegraded
}

// NewRunID returns a valid OpenTelemetry trace id for use as a Goobers run id.
func NewRunID() (string, error) {
	var b [16]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			return "", fmt.Errorf("generate run trace id: %w", err)
		}
		id, err := trace.TraceIDFromHex(hex.EncodeToString(b[:]))
		if err == nil && id.IsValid() {
			return id.String(), nil
		}
	}
}

// StartRun starts the root span for a workflow run.
func (c *Client) StartRun(ctx context.Context, attrs RunAttributes) (context.Context, Span, error) {
	if err := validateCommon(attrs.Gaggle, attrs.WorkflowID, attrs.RunID); err != nil {
		return ctx, Span{}, err
	}
	traceID, err := parseTraceID(attrs.RunID)
	if err != nil {
		return ctx, Span{}, err
	}
	ctx = contextWithRequestedTraceID(ctx, traceID)
	opts := []trace.SpanStartOption{
		trace.WithNewRoot(),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(scrubAttributes(c.scrubber, runAttributeSet(attrs))...),
	}
	opts = appendStartTime(opts, attrs.StartedAt)
	ctx, span := c.tracer.Start(ctx, redactWith(c.scrubber, runSpanName(attrs.WorkflowID)), opts...)
	metrics := c.beginSpanMetrics(SpanKindRun, attrs.StartedAt, runAttributeSet(attrs))
	return ctx, Span{span: span, scrubber: c.scrubber, metrics: metrics}, nil
}

// StartTask starts a task span under the current run context.
func (c *Client) StartTask(ctx context.Context, attrs TaskAttributes) (context.Context, Span, error) {
	if err := validateCommon(attrs.Gaggle, attrs.WorkflowID, attrs.RunID); err != nil {
		return ctx, Span{}, err
	}
	if attrs.TaskID == "" {
		return ctx, Span{}, errors.New("telemetry task span requires task id")
	}
	var err error
	ctx, err = contextWithRunTraceID(ctx, attrs.RunID)
	if err != nil {
		return ctx, Span{}, err
	}
	taskOpts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(scrubAttributes(c.scrubber, taskAttributeSet(attrs))...),
	}
	taskOpts = appendStartTime(taskOpts, attrs.StartedAt)
	ctx, span := c.tracer.Start(ctx, redactWith(c.scrubber, taskSpanName(attrs.TaskID)), taskOpts...)
	metrics := c.beginSpanMetrics(SpanKindTask, attrs.StartedAt, taskAttributeSet(attrs))
	metrics.recordRetry(attrs.Attempt, attrs.AttemptKind)
	return ctx, Span{span: span, scrubber: c.scrubber, metrics: metrics}, nil
}

// StartGate starts a gate evaluation span under the current run context.
func (c *Client) StartGate(ctx context.Context, attrs GateAttributes) (context.Context, Span, error) {
	if err := validateCommon(attrs.Gaggle, attrs.WorkflowID, attrs.RunID); err != nil {
		return ctx, Span{}, err
	}
	if attrs.GateID == "" {
		return ctx, Span{}, errors.New("telemetry gate span requires gate id")
	}
	var err error
	ctx, err = contextWithRunTraceID(ctx, attrs.RunID)
	if err != nil {
		return ctx, Span{}, err
	}
	gateOpts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(scrubAttributes(c.scrubber, gateAttributeSet(attrs))...),
	}
	gateOpts = appendStartTime(gateOpts, attrs.StartedAt)
	ctx, span := c.tracer.Start(ctx, redactWith(c.scrubber, gateSpanName(attrs.GateID)), gateOpts...)
	metrics := c.beginSpanMetrics(SpanKindGate, attrs.StartedAt, gateAttributeSet(attrs))
	return ctx, Span{span: span, scrubber: c.scrubber, metrics: metrics}, nil
}

// StartSchedulerSpan starts a scheduler decision span.
func (c *Client) StartSchedulerSpan(ctx context.Context, attrs SchedulerAttributes) (context.Context, Span, error) {
	if attrs.Gaggle == "" {
		return ctx, Span{}, errors.New("telemetry scheduler span requires gaggle")
	}
	if attrs.WorkflowID == "" {
		return ctx, Span{}, errors.New("telemetry scheduler span requires workflow id")
	}
	if attrs.Action == "" {
		return ctx, Span{}, errors.New("telemetry scheduler span requires action")
	}
	if attrs.RunID != "" {
		var err error
		ctx, err = contextWithRunTraceID(ctx, attrs.RunID)
		if err != nil {
			return ctx, Span{}, err
		}
	}
	ctx, span := c.tracer.Start(ctx, redactWith(c.scrubber, schedulerSpanName(attrs.Action)),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(scrubAttributes(c.scrubber, schedulerAttributeSet(attrs))...),
	)
	metrics := c.beginSpanMetrics(SpanKindScheduler, time.Time{}, schedulerAttributeSet(attrs))
	return ctx, Span{span: span, scrubber: c.scrubber, metrics: metrics}, nil
}

// Flush forces pending telemetry through configured providers. A transient
// export failure to an unreachable remote collector is best-effort and does not
// fail the caller (isCollectorUnreachable, #1124); a local-exporter error still
// propagates.
func (c *Client) Flush(ctx context.Context) error {
	if c.tracerProvider != nil {
		if err := c.tracerProvider.ForceFlush(ctx); err != nil {
			if !isCollectorUnreachable(err) {
				return fmt.Errorf("flush telemetry traces: %w", err)
			}
		} else if c.exporterHealth != nil && c.exporterHealth.TraceExporterInstalled() {
			c.exporterHealth.RecordTraceSuccess()
		}
	}
	// Metric export is strictly best-effort: unlike traces it has no local
	// exporter whose failure means a real defect, so every error here is a
	// remote-collector condition (unreachable, or a collector configured for
	// traces only). The SDK still reports it through the global otel error
	// handler; it must never fail the caller's work.
	if c.meterProvider != nil {
		if err := c.meterProvider.ForceFlush(ctx); err == nil && c.exporterHealth != nil && c.exporterHealth.MetricExporterInstalled() {
			c.exporterHealth.RecordMetricSuccess()
		}
	}
	if c.journalLogs != nil {
		if c.journalCatchup != nil {
			_ = c.journalCatchup.flush(ctx)
		}
		// Like metrics, journal Logs are remote-only and best-effort. The
		// pipeline reports failures independently of the journal writer.
		_ = c.journalLogs.flush(ctx)
	}
	return nil
}

// FlushLocal forces pending spans through the caller-provided exporter without
// waiting for a separately configured remote exporter.
func (c *Client) FlushLocal(ctx context.Context) error {
	if c.localSpanProcessor == nil {
		return nil
	}
	if err := c.localSpanProcessor.ForceFlush(ctx); err != nil {
		return fmt.Errorf("flush local telemetry traces: %w", err)
	}
	return nil
}

// Shutdown flushes and shuts down telemetry providers. As with Flush, a
// transient export failure to an unreachable remote collector is best-effort
// and does not fail the caller (isCollectorUnreachable, #1124) — the providers
// are still shut down; only the spurious error is dropped — while a
// local-exporter error still propagates.
func (c *Client) Shutdown(ctx context.Context) error {
	if c.unregisterJournal != nil {
		c.unregisterJournal()
	}
	if c.journalCatchup != nil {
		c.journalCatchup.shutdown(ctx)
	}
	var errs []error
	if c.tracerProvider != nil {
		if err := c.tracerProvider.Shutdown(ctx); err != nil {
			if !isCollectorUnreachable(err) {
				errs = append(errs, fmt.Errorf("shutdown telemetry traces: %w", err))
			}
		} else if c.exporterHealth != nil && c.exporterHealth.TraceExporterInstalled() {
			c.exporterHealth.RecordTraceSuccess()
		}
	}
	if c.journalLogs != nil {
		_ = c.journalLogs.shutdown(ctx)
	}
	// Journal shutdown can create the final stopping/shutdown drop counts, so
	// it must settle them before the meter provider performs its final export.
	// If the journal consumed the caller's deadline while abandoning a blocked
	// backlog, give that final metric export its own bounded best-effort window;
	// otherwise passing the already-cancelled context guarantees the new counts
	// can never leave the process.
	if c.meterProvider != nil {
		metricCtx := ctx
		cancel := func() {}
		if ctx.Err() != nil {
			// The caller's shutdown budget has already elapsed, so this is a
			// narrow last-chance export rather than a second full transport
			// timeout. Keep the whole client inside its bounded-shutdown
			// contract even when the metric destination is also unresponsive.
			metricCtx, cancel = context.WithTimeout(context.Background(), time.Second)
		}
		if err := c.meterProvider.Shutdown(metricCtx); err == nil && c.exporterHealth != nil && c.exporterHealth.MetricExporterInstalled() {
			c.exporterHealth.RecordMetricSuccess()
		}
		cancel()
	}
	return errors.Join(errs...)
}

// ExporterHealthSnapshot returns the client's bounded local exporter-health
// view. A nil/disabled client reports telemetry disabled explicitly.
func (c *Client) ExporterHealthSnapshot() ExporterHealthSnapshot {
	if c == nil || c.exporterHealth == nil {
		return DisabledExporterHealthSnapshot()
	}
	return c.exporterHealth.Snapshot()
}

func spanExporters(ctx context.Context, cfg Config) ([]sdktrace.SpanExporter, error) {
	var exporters []sdktrace.SpanExporter
	if cfg.SpanExporter != nil {
		exporters = append(exporters, cfg.SpanExporter)
	}
	if cfg.Exporter == "" && len(exporters) != 0 {
		if cfg.AzureMonitorConnectionString == "" || !cfg.AzureMonitorTraces {
			return exporters, nil
		}
		azure, err := newAzureMonitorSpanExporter(cfg.AzureMonitorConnectionString, cfg.AzureMonitorHTTPClient, cfg.AzureMonitorHostIdentity, cfg.azureReplayConfig("traces"))
		if err != nil {
			return exporters, err
		}
		var remote sdktrace.SpanExporter = azure
		if cfg.ExporterHealth != nil {
			cfg.ExporterHealth.ConfigureTrace()
			remote = observedSpanExporter{next: remote, health: cfg.ExporterHealth}
		}
		return append(exporters, remote), nil
	}
	if cfg.Exporter == "" && cfg.AzureMonitorConnectionString != "" && cfg.AzureMonitorTraces {
		azure, err := newAzureMonitorSpanExporter(cfg.AzureMonitorConnectionString, cfg.AzureMonitorHTTPClient, cfg.AzureMonitorHostIdentity, cfg.azureReplayConfig("traces"))
		if err != nil {
			return nil, err
		}
		var remote sdktrace.SpanExporter = azure
		if cfg.ExporterHealth != nil {
			cfg.ExporterHealth.ConfigureTrace()
			remote = observedSpanExporter{next: remote, health: cfg.ExporterHealth}
		}
		return append(exporters, remote), nil
	}
	if cfg.Exporter == "" && cfg.AzureMonitorConnectionString != "" {
		// A health/journal profile intentionally has no remote span exporter.
		// Do not fall through to the developer stdout default merely because the
		// same destination is used by diagnostics or journal logs.
		return exporters, nil
	}

	var exporter sdktrace.SpanExporter
	switch cfg.Exporter {
	case "", ExporterStdout:
		opts := []stdouttrace.Option{stdouttrace.WithPrettyPrint()}
		if cfg.Stdout != nil {
			opts = append(opts, stdouttrace.WithWriter(cfg.Stdout))
		} else {
			opts = append(opts, stdouttrace.WithWriter(os.Stdout))
		}
		var err error
		exporter, err = stdouttrace.New(opts...)
		if err != nil {
			return nil, fmt.Errorf("create stdout telemetry exporter: %w", err)
		}
	case ExporterOTLP:
		endpoint := strings.TrimSpace(cfg.OTLPEndpoint)
		if endpoint == "" {
			return nil, errors.New("create otlp telemetry exporter: endpoint must be explicitly configured")
		}
		opts := []otlptracegrpc.Option{}
		if strings.Contains(endpoint, "://") {
			opts = append(opts, otlptracegrpc.WithEndpointURL(endpoint))
		} else {
			opts = append(opts, otlptracegrpc.WithEndpoint(endpoint))
		}
		if cfg.OTLPInsecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		} else {
			tlsConfig, tlsErr := buildOTLPTLSConfig(cfg)
			if tlsErr != nil {
				// The exporter cannot be built, but exporters collected so
				// far (cfg.SpanExporter's local journal export, if
				// configured) are untouched — see ErrOTLPUnavailable's doc.
				if cfg.ExporterHealth != nil {
					cfg.ExporterHealth.RecordTraceFailure(tlsErr)
				}
				return exporters, fmt.Errorf("%w: %w", ErrOTLPUnavailable, tlsErr)
			}
			opts = append(opts, otlptracegrpc.WithTLSCredentials(credentials.NewTLS(tlsConfig)))
		}
		headers := make(map[string]string, len(cfg.OTLPHeaders))
		for name, value := range cfg.OTLPHeaders {
			headers[name] = value
		}
		opts = append(opts, otlptracegrpc.WithHeaders(headers))
		var err error
		exporter, err = otlptracegrpc.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("create otlp telemetry exporter: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported telemetry exporter %q", cfg.Exporter)
	}
	exporters = append(exporters, exporter)
	if cfg.ExporterHealth != nil && cfg.Exporter == ExporterOTLP {
		cfg.ExporterHealth.ConfigureTrace()
		exporters[len(exporters)-1] = observedSpanExporter{next: exporters[len(exporters)-1], health: cfg.ExporterHealth}
	}
	if cfg.AzureMonitorConnectionString != "" && cfg.AzureMonitorTraces {
		azure, err := newAzureMonitorSpanExporter(cfg.AzureMonitorConnectionString, cfg.AzureMonitorHTTPClient, cfg.AzureMonitorHostIdentity, cfg.azureReplayConfig("traces"))
		if err != nil {
			return exporters, err
		}
		var remote sdktrace.SpanExporter = azure
		if cfg.ExporterHealth != nil {
			cfg.ExporterHealth.ConfigureTrace()
			remote = observedSpanExporter{next: remote, health: cfg.ExporterHealth}
		}
		exporters = append(exporters, remote)
	}
	return exporters, nil
}

// buildOTLPTLSConfig assembles the OTLP exporter's client TLS config: with
// no CAFile it leaves RootCAs nil (the pre-#3804 default-verifier path,
// unchanged); with one configured it builds a system trust pool
// (SystemCertPool already returns a defensive copy safe to mutate — "clone"
// in #3804's design) plus the extra root. Also applies an optional
// SNI/verification override and an optional client certificate for mTLS.
// MinVersion stays TLS 1.2, unchanged from the path this extends.
func buildOTLPTLSConfig(cfg Config) (*tls.Config, error) {
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}
	// RootCAs stays nil (pre-#3804 behavior) unless an extra CA is actually
	// configured. On darwin/windows/ios, crypto/x509 hands verification to
	// the platform verifier when opts.Roots == nil but falls through to Go's
	// own verifier against RootCAs once it is non-nil — so unconditionally
	// setting a SystemCertPool clone here, even with nothing appended to it,
	// would silently swap in a different (and more permissive: CT policy,
	// OS distrust lists, and name constraints all go unevaluated) trust
	// decision on the tls-block-absent path this PR claims is untouched.
	if cfg.OTLPCAFile != "" {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		pem, err := os.ReadFile(cfg.OTLPCAFile)
		if err != nil {
			return nil, fmt.Errorf("read ca file %q: %w", cfg.OTLPCAFile, err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca file %q: no certificates found", cfg.OTLPCAFile)
		}
		tlsConfig.RootCAs = pool
	}
	if cfg.OTLPServerName != "" {
		tlsConfig.ServerName = cfg.OTLPServerName
	}
	if cfg.OTLPCertFile != "" || cfg.OTLPKeyFile != "" {
		pair, err := tls.LoadX509KeyPair(cfg.OTLPCertFile, cfg.OTLPKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load client certificate %q/%q: %w", cfg.OTLPCertFile, cfg.OTLPKeyFile, err)
		}
		tlsConfig.Certificates = []tls.Certificate{pair}
	}
	return tlsConfig, nil
}

func resourceAttrs(cfg Config) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 4+len(cfg.ResourceAttributes))
	if cfg.ServiceName != "" {
		attrs = append(attrs, attribute.String("service.name", cfg.ServiceName))
	}
	if cfg.ServiceVersion != "" {
		attrs = append(attrs, attribute.String("service.version", cfg.ServiceVersion))
	}
	if cfg.BuildCommit != "" {
		attrs = append(attrs, attribute.String("goobers.build.commit", cfg.BuildCommit))
	}
	if cfg.Environment != "" {
		attrs = append(attrs, attribute.String("deployment.environment", cfg.Environment))
	}
	return append(attrs, cfg.ResourceAttributes...)
}

func validateCommon(gaggle, workflowID, runID string) error {
	if gaggle == "" {
		return errors.New("telemetry span requires gaggle")
	}
	if workflowID == "" {
		return errors.New("telemetry span requires workflow id")
	}
	if runID == "" {
		return errors.New("telemetry span requires run id")
	}
	return nil
}

// appendStartTime backdates a span when an explicit start time is supplied.
// Zero means "stamp it now", which is every live tier-1 call site.
func appendStartTime(opts []trace.SpanStartOption, at time.Time) []trace.SpanStartOption {
	if at.IsZero() {
		return opts
	}
	return append(opts, trace.WithTimestamp(at))
}
