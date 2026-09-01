package telemetry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	apimetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric"
	"google.golang.org/grpc/credentials"
)

// The Goobers metric catalog. Every instrument here is documented in
// internal/telemetry/README.md with its type, unit, and allowed attributes;
// the collector pipeline is configured against those names.
const (
	// MetricRunDuration measures wall-clock workflow run duration.
	MetricRunDuration = "goobers.run.duration"
	// MetricRunOutcomes counts finished workflow runs by outcome.
	MetricRunOutcomes = "goobers.run.outcomes"
	// MetricStageDuration measures task/gate/scheduler stage duration.
	MetricStageDuration = "goobers.stage.duration"
	// MetricStageOutcomes counts finished stages by outcome.
	MetricStageOutcomes = "goobers.stage.outcomes"
	// MetricStageRetries counts stage attempts beyond the first.
	MetricStageRetries = "goobers.stage.retries"
	// MetricGateDecisions counts evaluated gate decisions.
	MetricGateDecisions = "goobers.gate.decisions"
	// MetricEscalations counts stages and gates that escalated to a human.
	MetricEscalations = "goobers.escalations"
	// MetricWorkActive reports in-flight runs and stages.
	MetricWorkActive = "goobers.work.active"
	// MetricStageMetricValue reports stage-emitted metrics.jsonl values.
	MetricStageMetricValue = "goobers.stage.metric.value"
)

const (
	// MetricAttrSpanKind distinguishes run/task/gate/scheduler work on
	// MetricWorkActive, which has no stage type of its own for run spans.
	MetricAttrSpanKind = "goobers.span.kind"

	metricNameAttribute = "goobers.metric.name"

	// metricExportInterval is the periodic reader's collect-and-push period.
	metricExportInterval = 60 * time.Second

	// metricAttributeMaxValues bounds the distinct values one metric
	// attribute key may carry per process; everything past the bound
	// collapses into metricAttributeOverflow so a mislabeled workflow or a
	// stage-authored metric name cannot blow up collector cardinality.
	metricAttributeMaxValues = 100
	metricAttributeMaxLength = 64
	metricAttributeOverflow  = "other"

	// outcomeUnset labels a span finished without a business outcome, so a
	// statusless End is never miscounted as a success.
	outcomeUnset = "unset"
)

// metricAttributeAllowlist is the closed set of span attribute keys allowed to
// become metric dimensions. Everything absent here — run ids, item ids and
// URLs, gaggle (a repository name), worktree ids, digests, branch indexes,
// error messages, agent/model usage detail — is either unbounded or sensitive
// and stays on spans only.
var metricAttributeAllowlist = map[string]struct{}{
	AttrWorkflow:         {},
	AttrStage:            {},
	AttrStageType:        {},
	AttrOutcome:          {},
	AttrErrorCode:        {},
	AttrAttemptKind:      {},
	AttrGateDecision:     {},
	AttrModel:            {},
	AttrStorageOperation: {},
	metricNameAttribute:  {},
	metricUnitAttribute:  {},
	MetricAttrSpanKind:   {},
}

// instruments owns the process-wide Goobers metric instruments. A nil
// *instruments disables metric recording, which is what every client built
// without a metric reader gets.
type instruments struct {
	runDuration   apimetric.Float64Histogram
	runOutcomes   apimetric.Int64Counter
	stageDuration apimetric.Float64Histogram
	stageOutcomes apimetric.Int64Counter
	stageRetries  apimetric.Int64Counter
	gateDecisions apimetric.Int64Counter
	escalations   apimetric.Int64Counter
	activeWork    apimetric.Int64UpDownCounter
	stageMetrics  apimetric.Float64Histogram
	worktreeBytes apimetric.Int64Gauge
	workcopyBytes apimetric.Int64Gauge
	limiter       *cardinalityLimiter
}

func newInstruments(meter apimetric.Meter) (*instruments, error) {
	var errs []error
	record := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	inst := &instruments{limiter: newCardinalityLimiter(metricAttributeMaxValues)}
	var err error

	inst.runDuration, err = meter.Float64Histogram(MetricRunDuration,
		apimetric.WithUnit("s"), apimetric.WithDescription("Workflow run duration."))
	record(err)
	inst.runOutcomes, err = meter.Int64Counter(MetricRunOutcomes,
		apimetric.WithUnit("{run}"), apimetric.WithDescription("Finished workflow runs by outcome."))
	record(err)
	inst.stageDuration, err = meter.Float64Histogram(MetricStageDuration,
		apimetric.WithUnit("s"), apimetric.WithDescription("Workflow stage duration."))
	record(err)
	inst.stageOutcomes, err = meter.Int64Counter(MetricStageOutcomes,
		apimetric.WithUnit("{stage}"), apimetric.WithDescription("Finished workflow stages by outcome."))
	record(err)
	inst.stageRetries, err = meter.Int64Counter(MetricStageRetries,
		apimetric.WithUnit("{attempt}"), apimetric.WithDescription("Stage attempts beyond the first."))
	record(err)
	inst.gateDecisions, err = meter.Int64Counter(MetricGateDecisions,
		apimetric.WithUnit("{decision}"), apimetric.WithDescription("Evaluated gate decisions."))
	record(err)
	inst.escalations, err = meter.Int64Counter(MetricEscalations,
		apimetric.WithUnit("{escalation}"), apimetric.WithDescription("Stages and gates escalated to a human."))
	record(err)
	inst.activeWork, err = meter.Int64UpDownCounter(MetricWorkActive,
		apimetric.WithUnit("{span}"), apimetric.WithDescription("In-flight runs and stages."))
	record(err)
	inst.stageMetrics, err = meter.Float64Histogram(MetricStageMetricValue,
		apimetric.WithUnit("1"), apimetric.WithDescription("Stage-emitted metrics.jsonl values."))
	record(err)
	inst.worktreeBytes, err = meter.Int64Gauge(EventWorktreeDiskUsage,
		apimetric.WithUnit("By"), apimetric.WithDescription("Apparent bytes of one managed worktree."))
	record(err)
	inst.workcopyBytes, err = meter.Int64Gauge(EventWorkcopyDiskUsage,
		apimetric.WithUnit("By"), apimetric.WithDescription("Aggregate apparent bytes of managed workcopies."))
	record(err)

	if len(errs) != 0 {
		return nil, errors.Join(errs...)
	}
	return inst, nil
}

// cardinalityLimiter caps the distinct values one metric attribute key may
// take. It is the second half of the bounded-cardinality contract: the
// allowlist decides which keys ship, the limiter decides how many values a
// shipped key may have before collapsing into metricAttributeOverflow.
type cardinalityLimiter struct {
	mu     sync.Mutex
	limit  int
	values map[string]map[string]struct{}
}

func newCardinalityLimiter(limit int) *cardinalityLimiter {
	return &cardinalityLimiter{limit: limit, values: make(map[string]map[string]struct{})}
}

func (l *cardinalityLimiter) bound(key, value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	if !boundedMetricValue(key, value) {
		return metricAttributeOverflow, true
	}
	if l == nil {
		return value, true
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	seen, exists := l.values[key]
	if !exists {
		seen = make(map[string]struct{})
		l.values[key] = seen
	}
	if _, known := seen[value]; known {
		return value, true
	}
	if len(seen) >= l.limit {
		return metricAttributeOverflow, true
	}
	seen[value] = struct{}{}
	return value, true
}

// boundedMetricValue rejects values that cannot be a stable dimension: long
// values, and anything outside an identifier-ish character set (which is how
// free text, prompts, paths, and URLs are kept out even when they arrive
// under an allowlisted key). Units are the one exception: UCUM annotations
// such as "{file}" and rates such as "By/s" are still closed vocabulary.
func boundedMetricValue(key, value string) bool {
	if len(value) > metricAttributeMaxLength {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		case key == metricUnitAttribute && (r == '{' || r == '}' || r == '/' || r == '%'):
		default:
			return false
		}
	}
	return true
}

// metricAttributes projects span attributes onto the bounded metric dimension
// set: allowlisted keys with string values only, each value cardinality-capped.
func (i *instruments) metricAttributes(attrs []attribute.KeyValue) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(attrs))
	for _, attr := range attrs {
		key := string(attr.Key)
		if _, allowed := metricAttributeAllowlist[key]; !allowed {
			continue
		}
		if attr.Value.Type() != attribute.STRING {
			continue
		}
		value, ok := i.limiter.bound(key, attr.Value.AsString())
		if !ok {
			continue
		}
		out = append(out, attribute.String(key, value))
	}
	return out
}

func (i *instruments) attributeSet(pairs ...attribute.KeyValue) apimetric.MeasurementOption {
	return apimetric.WithAttributes(i.metricAttributes(pairs)...)
}

// spanMetrics carries the bounded metric dimensions of a live span so the
// span's terminal call can record duration and outcome without re-deriving
// them from unbounded span attributes.
type spanMetrics struct {
	inst    *instruments
	attrs   []attribute.KeyValue
	kind    string
	started time.Time
}

func (c *Client) beginSpanMetrics(kind string, startedAt time.Time, attrs []attribute.KeyValue) *spanMetrics {
	if c == nil || c.instruments == nil {
		return nil
	}
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	m := &spanMetrics{
		inst:    c.instruments,
		attrs:   c.instruments.metricAttributes(attrs),
		kind:    kind,
		started: startedAt,
	}
	m.inst.activeWork.Add(context.Background(), 1, apimetric.WithAttributes(m.activeAttributes()...))
	return m
}

func (m *spanMetrics) activeAttributes() []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 2)
	for _, attr := range m.attrs {
		if attr.Key == AttrWorkflow {
			attrs = append(attrs, attr)
		}
	}
	kind, ok := m.inst.limiter.bound(MetricAttrSpanKind, m.kind)
	if ok {
		attrs = append(attrs, attribute.String(MetricAttrSpanKind, kind))
	}
	return attrs
}

// record closes out a span's metrics: duration, outcome, escalation, and the
// matching active-work decrement. endedAt zero means "now", mirroring
// Span.EndAt.
func (m *spanMetrics) record(endedAt time.Time, outcome, errorCode string) {
	if m == nil {
		return
	}
	ctx := context.Background()
	if endedAt.IsZero() {
		endedAt = time.Now()
	}
	seconds := endedAt.Sub(m.started).Seconds()
	if seconds < 0 {
		seconds = 0
	}

	attrs := append(append([]attribute.KeyValue(nil), m.attrs...),
		attribute.String(AttrOutcome, outcome),
	)
	if errorCode != "" {
		attrs = append(attrs, attribute.String(AttrErrorCode, errorCode))
	}
	set := m.inst.attributeSet(attrs...)

	if m.kind == SpanKindRun {
		m.inst.runDuration.Record(ctx, seconds, set)
		m.inst.runOutcomes.Add(ctx, 1, set)
	} else {
		m.inst.stageDuration.Record(ctx, seconds, set)
		m.inst.stageOutcomes.Add(ctx, 1, set)
	}
	if isEscalation(outcome) || isEscalation(errorCode) {
		m.inst.escalations.Add(ctx, 1, m.inst.attributeSet(m.attrs...))
	}
	m.inst.activeWork.Add(ctx, -1, apimetric.WithAttributes(m.activeAttributes()...))
}

func (m *spanMetrics) recordRetry(attempt int, attemptKind string) {
	if m == nil || attempt <= 1 {
		return
	}
	attrs := append([]attribute.KeyValue(nil), m.attrs...)
	if attemptKind != "" {
		attrs = append(attrs, attribute.String(AttrAttemptKind, attemptKind))
	}
	m.inst.stageRetries.Add(context.Background(), 1, m.inst.attributeSet(attrs...))
}

func (m *spanMetrics) recordGateDecision(decision string) {
	if m == nil || decision == "" {
		return
	}
	attrs := append(append([]attribute.KeyValue(nil), m.attrs...),
		attribute.String(AttrGateDecision, decision),
	)
	ctx := context.Background()
	m.inst.gateDecisions.Add(ctx, 1, m.inst.attributeSet(attrs...))
	if isEscalation(decision) {
		m.inst.escalations.Add(ctx, 1, m.inst.attributeSet(m.attrs...))
	}
}

// recordStageMetric exports one stage-emitted metrics.jsonl value. The
// stage-authored name is a dimension, not an instrument name, so a stage
// cannot mint unbounded instruments in the collector.
func (m *spanMetrics) recordStageMetric(name, unit string, value float64) {
	if m == nil || name == "" {
		return
	}
	attrs := append(append([]attribute.KeyValue(nil), m.attrs...),
		attribute.String(metricNameAttribute, name),
	)
	if unit != "" {
		attrs = append(attrs, attribute.String(metricUnitAttribute, unit))
	}
	m.inst.stageMetrics.Record(context.Background(), value, m.inst.attributeSet(attrs...))
}

func isEscalation(value string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "escalat")
}

// recordDiskUsage publishes a managed-storage measurement as a gauge. The
// gauge selector keeps the bounded worktree/workcopy split explicit while the
// nil-instrument guard lives in one place.
func (c *Client) recordDiskUsage(ctx context.Context, gauge diskUsageGauge, value int64, attrs []attribute.KeyValue) {
	if c == nil || c.instruments == nil {
		return
	}
	instrument := c.instruments.worktreeBytes
	if gauge == workcopyDiskUsage {
		instrument = c.instruments.workcopyBytes
	}
	instrument.Record(ctx, value, c.instruments.attributeSet(attrs...))
}

// diskUsageGauge selects which managed-storage gauge a measurement belongs to.
type diskUsageGauge int

const (
	worktreeDiskUsage diskUsageGauge = iota
	workcopyDiskUsage
)

// metricReaders assembles the readers attached to the meter provider: a
// caller-supplied reader (tests), a caller-supplied exporter behind a periodic
// reader, and the OTLP exporter behind a periodic reader when OTLP export is
// configured. An OTLP TLS failure degrades exactly like the trace exporter's:
// the readers built so far are returned alongside ErrOTLPUnavailable.
func metricReaders(ctx context.Context, cfg Config) ([]metric.Reader, error) {
	var readers []metric.Reader
	if cfg.MetricReader != nil {
		readers = append(readers, cfg.MetricReader)
	}
	if cfg.MetricExporter != nil {
		readers = append(readers, metric.NewPeriodicReader(cfg.MetricExporter,
			metric.WithInterval(metricInterval(cfg))))
	}
	if cfg.Exporter != ExporterOTLP {
		return readers, nil
	}
	exporter, err := metricExporter(ctx, cfg)
	if err != nil {
		return readers, err
	}
	return append(readers, metric.NewPeriodicReader(exporter, metric.WithInterval(metricInterval(cfg)))), nil
}

func metricInterval(cfg Config) time.Duration {
	if cfg.MetricExportInterval > 0 {
		return cfg.MetricExportInterval
	}
	return metricExportInterval
}

// metricExporter builds the OTLP/gRPC metric exporter from the same
// explicitly configured endpoint, TLS material, and headers as the trace
// exporter (SEC-048: no built-in destination).
func metricExporter(ctx context.Context, cfg Config) (metric.Exporter, error) {
	endpoint := strings.TrimSpace(cfg.OTLPEndpoint)
	if endpoint == "" {
		return nil, errors.New("create otlp telemetry metric exporter: endpoint must be explicitly configured")
	}
	opts := []otlpmetricgrpc.Option{}
	if strings.Contains(endpoint, "://") {
		opts = append(opts, otlpmetricgrpc.WithEndpointURL(endpoint))
	} else {
		opts = append(opts, otlpmetricgrpc.WithEndpoint(endpoint))
	}
	if cfg.OTLPInsecure {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	} else {
		tlsConfig, tlsErr := buildOTLPTLSConfig(cfg)
		if tlsErr != nil {
			return nil, fmt.Errorf("%w: %w", ErrOTLPUnavailable, tlsErr)
		}
		opts = append(opts, otlpmetricgrpc.WithTLSCredentials(credentials.NewTLS(tlsConfig)))
	}
	headers := make(map[string]string, len(cfg.OTLPHeaders))
	for name, value := range cfg.OTLPHeaders {
		headers[name] = value
	}
	opts = append(opts, otlpmetricgrpc.WithHeaders(headers))
	exporter, err := otlpmetricgrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("create otlp telemetry metric exporter: %w", err)
	}
	return exporter, nil
}
