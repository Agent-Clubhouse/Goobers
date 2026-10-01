package telemetry

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ExporterHealth tracks locally observable telemetry exporter health.
type ExporterHealth struct {
	mu       sync.Mutex
	enabled  bool
	mode     string
	endpoint endpointHealth
	trace    exporterSignalHealth
	metric   exporterSignalHealth
}

type endpointHealth struct {
	host  string
	class string
}

// ExporterHealthSnapshot is a scrubbed, bounded health view suitable for
// daemon-local API and diagnostic surfaces.
type ExporterHealthSnapshot struct {
	Enabled       bool                 `json:"enabled"`
	Mode          string               `json:"mode,omitempty"`
	EndpointHost  string               `json:"endpointHost,omitempty"`
	EndpointClass string               `json:"endpointClass,omitempty"`
	Trace         ExporterSignalStatus `json:"trace"`
	Metric        ExporterSignalStatus `json:"metric"`
}

// ExporterSignalStatus reports health for one telemetry signal.
type ExporterSignalStatus struct {
	Configured              bool       `json:"configured"`
	State                   string     `json:"state"`
	LastSuccessAt           *time.Time `json:"lastSuccessAt,omitempty"`
	LastFailureAt           *time.Time `json:"lastFailureAt,omitempty"`
	LastFailureReason       string     `json:"lastFailureReason,omitempty"`
	ConsecutiveFailures     uint64     `json:"consecutiveFailures,omitempty"`
	LastTransitionAt        *time.Time `json:"lastTransitionAt,omitempty"`
	RecoveryTransitions     uint64     `json:"recoveryTransitions,omitempty"`
	FailureTransitions      uint64     `json:"failureTransitions,omitempty"`
	SuppressedFailureEvents uint64     `json:"suppressedFailureEvents,omitempty"`
}

type exporterSignalHealth struct {
	configured              bool
	exporterInstalled       bool
	unhealthy               bool
	lastSuccessAt           time.Time
	lastFailureAt           time.Time
	lastFailureReason       string
	consecutiveFailures     uint64
	lastTransitionAt        time.Time
	recoveryTransitions     uint64
	failureTransitions      uint64
	suppressedFailureEvents uint64
}

// NewExporterHealth creates the shared exporter-health monitor for a telemetry
// client. endpoint may contain a scheme or path; only the host classification
// is retained.
func NewExporterHealth(enabled bool, mode, endpoint string) *ExporterHealth {
	host, class := classifyEndpoint(endpoint)
	return &ExporterHealth{
		enabled: enabled,
		mode:    boundedMode(mode),
		endpoint: endpointHealth{
			host:  host,
			class: class,
		},
	}
}

// DisabledExporterHealthSnapshot reports an explicit disabled state without a
// live telemetry client.
func DisabledExporterHealthSnapshot() ExporterHealthSnapshot {
	return ExporterHealthSnapshot{
		Enabled: false,
		Mode:    "disabled",
		Trace:   ExporterSignalStatus{State: "disabled"},
		Metric:  ExporterSignalStatus{State: "disabled"},
	}
}

// ConfigureTrace marks the trace exporter as configured for later provider
// flush/shutdown observations.
func (h *ExporterHealth) ConfigureTrace() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.trace.configured = true
	h.trace.exporterInstalled = true
	h.mu.Unlock()
}

// ConfigureMetric marks the metric exporter as configured for later provider
// flush/shutdown observations.
func (h *ExporterHealth) ConfigureMetric() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.metric.configured = true
	h.metric.exporterInstalled = true
	h.mu.Unlock()
}

// TraceExporterInstalled reports whether a remote trace exporter was installed.
func (h *ExporterHealth) TraceExporterInstalled() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.trace.exporterInstalled
}

// MetricExporterInstalled reports whether a remote metric exporter was installed.
func (h *ExporterHealth) MetricExporterInstalled() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.metric.exporterInstalled
}

// RecordTraceSuccess records a successful trace exporter observation.
func (h *ExporterHealth) RecordTraceSuccess() { h.recordSuccess(&h.trace) }

// RecordMetricSuccess records a successful metric exporter observation.
func (h *ExporterHealth) RecordMetricSuccess() { h.recordSuccess(&h.metric) }

// RecordTraceFailure records a failed trace exporter observation.
func (h *ExporterHealth) RecordTraceFailure(err error) { h.recordFailure(&h.trace, err) }

// RecordMetricFailure records a failed metric exporter observation.
func (h *ExporterHealth) RecordMetricFailure(err error) { h.recordFailure(&h.metric, err) }

func (h *ExporterHealth) recordSuccess(signal *exporterSignalHealth) {
	if h == nil {
		return
	}
	now := time.Now().UTC()
	h.mu.Lock()
	defer h.mu.Unlock()
	signal.configured = true
	signal.lastSuccessAt = now
	signal.consecutiveFailures = 0
	if signal.unhealthy {
		signal.unhealthy = false
		signal.lastTransitionAt = now
		signal.recoveryTransitions++
	}
}

func (h *ExporterHealth) recordFailure(signal *exporterSignalHealth, err error) {
	if h == nil {
		return
	}
	now := time.Now().UTC()
	h.mu.Lock()
	defer h.mu.Unlock()
	signal.configured = true
	signal.lastFailureAt = now
	signal.lastFailureReason = exporterFailureReason(err)
	signal.consecutiveFailures++
	if signal.unhealthy {
		signal.suppressedFailureEvents++
		return
	}
	signal.unhealthy = true
	signal.lastTransitionAt = now
	signal.failureTransitions++
}

// Snapshot returns a copy of the current bounded health state.
func (h *ExporterHealth) Snapshot() ExporterHealthSnapshot {
	if h == nil {
		return DisabledExporterHealthSnapshot()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return ExporterHealthSnapshot{
		Enabled:       h.enabled,
		Mode:          h.mode,
		EndpointHost:  h.endpoint.host,
		EndpointClass: h.endpoint.class,
		Trace:         h.trace.snapshot(),
		Metric:        h.metric.snapshot(),
	}
}

func (s exporterSignalHealth) snapshot() ExporterSignalStatus {
	state := "disabled"
	if s.configured {
		state = "healthy"
		if s.unhealthy {
			state = "unhealthy"
		} else if s.lastSuccessAt.IsZero() {
			state = "unknown"
		}
	}
	return ExporterSignalStatus{
		Configured:              s.configured,
		State:                   state,
		LastSuccessAt:           timePtr(s.lastSuccessAt),
		LastFailureAt:           timePtr(s.lastFailureAt),
		LastFailureReason:       s.lastFailureReason,
		ConsecutiveFailures:     s.consecutiveFailures,
		LastTransitionAt:        timePtr(s.lastTransitionAt),
		RecoveryTransitions:     s.recoveryTransitions,
		FailureTransitions:      s.failureTransitions,
		SuppressedFailureEvents: s.suppressedFailureEvents,
	}
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func boundedMode(mode string) string {
	switch strings.TrimSpace(strings.ToLower(mode)) {
	case "", "disabled":
		return "disabled"
	case "local":
		return "local"
	case "azure-monitor":
		return "azure-monitor"
	case string(ExporterOTLP):
		return string(ExporterOTLP)
	case string(ExporterStdout):
		return string(ExporterStdout)
	default:
		return "custom"
	}
}

func classifyEndpoint(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	host := raw
	if parsed, err := url.Parse(raw); err == nil && parsed.Host != "" {
		host = parsed.Hostname()
	} else if h, _, err := net.SplitHostPort(raw); err == nil {
		host = h
	} else {
		host = strings.TrimPrefix(raw, "[")
		host = strings.TrimSuffix(host, "]")
		if i := strings.LastIndex(host, ":"); i >= 0 && strings.Count(host, ":") == 1 {
			host = host[:i]
		}
	}
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if host == "" {
		return "", "unknown"
	}
	if ip := net.ParseIP(host); ip != nil {
		return host, "ip-address"
	}
	if strings.EqualFold(host, "localhost") {
		return host, "localhost"
	}
	return host, "dns-name"
}

func exporterFailureReason(err error) string {
	if err == nil {
		return "unknown"
	}
	if status.Code(err) == codes.Unimplemented {
		return "signal_unimplemented"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	if isCollectorUnreachable(err) {
		return "collector_unavailable"
	}
	return "exporter_error"
}

type observedSpanExporter struct {
	next   sdktrace.SpanExporter
	health *ExporterHealth
}

func (e observedSpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	err := e.next.ExportSpans(ctx, spans)
	if err != nil {
		e.health.RecordTraceFailure(err)
	} else {
		e.health.RecordTraceSuccess()
	}
	return err
}

func (e observedSpanExporter) Shutdown(ctx context.Context) error {
	err := e.next.Shutdown(ctx)
	if err != nil {
		e.health.RecordTraceFailure(err)
	} else {
		e.health.RecordTraceSuccess()
	}
	return err
}

type observedMetricExporter struct {
	next   metric.Exporter
	health *ExporterHealth
}

func (e observedMetricExporter) Temporality(kind metric.InstrumentKind) metricdata.Temporality {
	return e.next.Temporality(kind)
}

func (e observedMetricExporter) Aggregation(kind metric.InstrumentKind) metric.Aggregation {
	return e.next.Aggregation(kind)
}

func (e observedMetricExporter) Export(ctx context.Context, data *metricdata.ResourceMetrics) error {
	err := e.next.Export(ctx, data)
	if err != nil {
		e.health.RecordMetricFailure(err)
	} else {
		e.health.RecordMetricSuccess()
	}
	return err
}

func (e observedMetricExporter) ForceFlush(ctx context.Context) error {
	err := e.next.ForceFlush(ctx)
	if err != nil {
		e.health.RecordMetricFailure(err)
	} else {
		e.health.RecordMetricSuccess()
	}
	return err
}

func (e observedMetricExporter) Shutdown(ctx context.Context) error {
	err := e.next.Shutdown(ctx)
	if err != nil {
		e.health.RecordMetricFailure(err)
	} else {
		e.health.RecordMetricSuccess()
	}
	return err
}
