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

	"github.com/goobers/goobers/internal/journal"
)

// ExporterHealth tracks locally observable telemetry exporter health.
type ExporterHealth struct {
	unavailableReason  string
	replayRoot         string
	journalSnapshot    func() JournalExportStats
	diagnosticSnapshot func() DiagnosticExportStats
	name               string
	destinations       map[string]*ExporterHealth
	mu                 sync.Mutex
	enabled            bool
	mode               string
	endpoint           endpointHealth
	journal            *journal.InstanceLog
	trace              exporterSignalHealth
	metric             exporterSignalHealth
	// now and refusedWindow are injectable for tests; zero values use the
	// wall clock and the export error log's suppression window.
	now           func() time.Time
	refusedWindow time.Duration
}

type endpointHealth struct {
	host  string
	class string
}

// ExporterHealthSnapshot is a scrubbed, bounded health view suitable for
// daemon-local API and diagnostic surfaces.
type ExporterHealthSnapshot struct {
	UnavailableReason string                            `json:"unavailableReason,omitempty"`
	Replay            *ExporterReplayHealthSnapshot     `json:"replay,omitempty"`
	Journal           *ExporterDeliveryCounters         `json:"journal,omitempty"`
	Diagnostics       *ExporterDeliveryCounters         `json:"diagnostics,omitempty"`
	Destinations      map[string]ExporterHealthSnapshot `json:"destinations,omitempty"`
	Enabled           bool                              `json:"enabled"`
	Mode              string                            `json:"mode,omitempty"`
	EndpointHost      string                            `json:"endpointHost,omitempty"`
	EndpointClass     string                            `json:"endpointClass,omitempty"`
	Trace             ExporterSignalStatus              `json:"trace"`
	Metric            ExporterSignalStatus              `json:"metric"`
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
	installedExporters      map[string]struct{}
	unhealthy               bool
	failingExporters        map[string]string
	lastSuccessAt           time.Time
	lastFailureAt           time.Time
	lastFailureReason       string
	consecutiveFailures     uint64
	lastTransitionAt        time.Time
	recoveryTransitions     uint64
	failureTransitions      uint64
	suppressedFailureEvents uint64
	journaledRecoveries     uint64
	journaledFailures       uint64
	// Persistent-failure instance-log events (#6417): the first failure, then
	// one per suppression window while the signal keeps failing.
	lastRefusedAt  time.Time
	refusedRepeats uint64
	pendingRefused *journal.Event
}

const (
	exporterHealthTransitionAnnotation = "telemetry.exporter.transition"
	exporterHealthRefusedAnnotation    = "telemetry.exporter.refused"
	// exporterHealthRefusedCode is the instance-log failure code for an
	// export that keeps failing, beside telemetry_otlp_unavailable.
	exporterHealthRefusedCode          = "telemetry_export_refused"
	exporterHealthStateUnhealthy       = "unhealthy"
	exporterHealthStateRecovered       = "recovered"
	exporterHealthExporterDefault      = "default"
	exporterHealthExporterOTLP         = "otlp"
	exporterHealthExporterAzureMonitor = "azure-monitor"
)

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

// AttachInstanceLog connects exporter-health transitions to the durable local
// instance journal. Any transition observed before the journal was available is
// written once when attached.
func (h *ExporterHealth) AttachInstanceLog(log *journal.InstanceLog) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.journal = log
	events := h.pendingTransitionEventsLocked()
	h.mu.Unlock()
	appendExporterHealthTransitions(log, events)
	for _, child := range h.destinationMonitors() {
		child.AttachInstanceLog(log)
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
	h.configureTraceExporter(exporterHealthExporterDefault)
}

func (h *ExporterHealth) configureTraceExporter(exporter string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.configureExporterLocked(&h.trace, exporter)
	h.mu.Unlock()
}

// ConfigureMetric marks the metric exporter as configured for later provider
// flush/shutdown observations.
func (h *ExporterHealth) ConfigureMetric() {
	h.configureMetricExporter(exporterHealthExporterDefault)
}

func (h *ExporterHealth) configureMetricExporter(exporter string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.configureExporterLocked(&h.metric, exporter)
	h.mu.Unlock()
}

func (h *ExporterHealth) configureExporterLocked(signal *exporterSignalHealth, exporter string) {
	signal.configured = true
	signal.exporterInstalled = true
	if signal.installedExporters == nil {
		signal.installedExporters = make(map[string]struct{})
	}
	signal.installedExporters[boundedExporterHealthExporter(exporter)] = struct{}{}
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
func (h *ExporterHealth) RecordTraceSuccess() {
	h.recordSuccess("trace", &h.trace, exporterHealthExporterDefault)
}

// RecordMetricSuccess records a successful metric exporter observation.
func (h *ExporterHealth) RecordMetricSuccess() {
	h.recordSuccess("metric", &h.metric, exporterHealthExporterDefault)
}

// RecordTraceFailure records a failed trace exporter observation.
func (h *ExporterHealth) RecordTraceFailure(err error) {
	h.recordFailure("trace", &h.trace, exporterHealthExporterDefault, err)
}

// RecordMetricFailure records a failed metric exporter observation.
func (h *ExporterHealth) RecordMetricFailure(err error) {
	h.recordFailure("metric", &h.metric, exporterHealthExporterDefault, err)
}

func (h *ExporterHealth) recordTraceExporterSuccess(exporter string) {
	h.recordSuccess("trace", &h.trace, exporter)
}

func (h *ExporterHealth) recordTraceProviderSuccess() {
	h.recordInstalledExportersSuccess("trace", &h.trace)
}

func (h *ExporterHealth) recordMetricExporterSuccess(exporter string) {
	h.recordSuccess("metric", &h.metric, exporter)
}

func (h *ExporterHealth) recordMetricProviderSuccess() {
	h.recordInstalledExportersSuccess("metric", &h.metric)
}

func (h *ExporterHealth) recordTraceExporterFailure(exporter string, err error) {
	h.recordFailure("trace", &h.trace, exporter, err)
}

func (h *ExporterHealth) recordMetricExporterFailure(exporter string, err error) {
	h.recordFailure("metric", &h.metric, exporter, err)
}

func (h *ExporterHealth) recordInstalledExportersSuccess(signalName string, signal *exporterSignalHealth) {
	if h == nil {
		return
	}
	exporters := h.installedExporters(signal)
	for _, exporter := range exporters {
		h.recordSuccess(signalName, signal, exporter)
	}
}

func (h *ExporterHealth) installedExporters(signal *exporterSignalHealth) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(signal.installedExporters) == 0 {
		if !signal.exporterInstalled {
			return nil
		}
		return []string{exporterHealthExporterDefault}
	}
	exporters := make([]string, 0, len(signal.installedExporters))
	for exporter := range signal.installedExporters {
		exporters = append(exporters, exporter)
	}
	return exporters
}

func (h *ExporterHealth) clock() time.Time {
	if h.now != nil {
		return h.now().UTC()
	}
	return time.Now().UTC()
}

func (h *ExporterHealth) recordSuccess(signalName string, signal *exporterSignalHealth, exporter string) {
	if h == nil {
		return
	}
	now := h.clock()
	h.mu.Lock()
	signal.configured = true
	signal.lastSuccessAt = now
	delete(signal.failingExporters, boundedExporterHealthExporter(exporter))
	var event *journal.Event
	log := h.journal
	if len(signal.failingExporters) > 0 {
		signal.unhealthy = true
		h.mu.Unlock()
		return
	}
	if signal.unhealthy {
		signal.unhealthy = false
		signal.consecutiveFailures = 0
		signal.lastTransitionAt = now
		signal.recoveryTransitions++
		// A later failure is a new first occurrence, not a repeat.
		signal.lastRefusedAt, signal.refusedRepeats = time.Time{}, 0
		event = h.transitionEventLocked(signalName, exporterHealthStateRecovered, signal.lastFailureReason, now)
		if log != nil {
			signal.journaledRecoveries = signal.recoveryTransitions
		}
	}
	h.mu.Unlock()
	appendExporterHealthTransition(log, event)
}

func (h *ExporterHealth) recordFailure(signalName string, signal *exporterSignalHealth, exporter string, err error) {
	if h == nil {
		return
	}
	now := h.clock()
	h.mu.Lock()
	signal.configured = true
	signal.lastFailureAt = now
	signal.lastFailureReason = exporterFailureReason(err)
	signal.consecutiveFailures++
	if signal.failingExporters == nil {
		signal.failingExporters = make(map[string]string)
	}
	signal.failingExporters[boundedExporterHealthExporter(exporter)] = signal.lastFailureReason
	if signal.unhealthy {
		signal.suppressedFailureEvents++
		refused := h.refusedEventLocked(signalName, signal, exporter, now)
		log := h.journal
		h.mu.Unlock()
		appendExporterHealthTransition(log, refused)
		return
	}
	signal.unhealthy = true
	signal.lastTransitionAt = now
	signal.failureTransitions++
	event := h.transitionEventLocked(signalName, exporterHealthStateUnhealthy, signal.lastFailureReason, now)
	refused := h.refusedEventLocked(signalName, signal, exporter, now)
	log := h.journal
	if log != nil {
		signal.journaledFailures = signal.failureTransitions
	}
	h.mu.Unlock()
	appendExporterHealthTransitions(log, []*journal.Event{event, refused})
}

// refusedEventLocked returns the telemetry_export_refused failure event for
// the first failure of a failing period and for the first repeat after each
// suppression window, mirroring the export error log's cadence. Repeats in
// between are counted into the next event. Before the instance log is
// attached, the latest due event is held for AttachInstanceLog.
func (h *ExporterHealth) refusedEventLocked(signalName string, signal *exporterSignalHealth, exporter string, now time.Time) *journal.Event {
	window := h.refusedWindow
	if window <= 0 {
		window = exportErrorRepeatWindow
	}
	if !signal.lastRefusedAt.IsZero() {
		signal.refusedRepeats++
		if now.Sub(signal.lastRefusedAt) < window {
			return nil
		}
	}
	runner := map[string]any{
		"annotation":  exporterHealthRefusedAnnotation,
		"signal":      signalName,
		"destination": boundedExporterHealthExporter(exporter),
		"errorClass":  signal.lastFailureReason,
		"mode":        h.mode,
	}
	if signal.refusedRepeats > 0 {
		runner["repeats"] = signal.refusedRepeats
		runner["window"] = window.String()
	}
	if h.endpoint.class != "" {
		runner["endpointClass"] = h.endpoint.class
	}
	event := &journal.Event{
		Type:   journal.EventError,
		Time:   now,
		Error:  &journal.ErrorDetail{Code: exporterHealthRefusedCode, Message: signal.lastFailureReason},
		Runner: runner,
	}
	if h.name != "" {
		event.Runner["destination"] = h.name
	}
	signal.lastRefusedAt, signal.refusedRepeats = now, 0
	if h.journal == nil {
		signal.pendingRefused = event
		return nil
	}
	return event
}

// Snapshot returns a copy of the current bounded health state.
func (h *ExporterHealth) Snapshot() ExporterHealthSnapshot {
	if h == nil {
		return DisabledExporterHealthSnapshot()
	}
	h.mu.Lock()
	snapshot := ExporterHealthSnapshot{
		UnavailableReason: h.unavailableReason,
		Enabled:           h.enabled,
		Mode:              h.mode,
		EndpointHost:      h.endpoint.host,
		EndpointClass:     h.endpoint.class,
		Trace:             h.trace.snapshot(),
		Metric:            h.metric.snapshot(),
	}
	replayRoot, journalSnapshot, diagnosticSnapshot := h.replayRoot, h.journalSnapshot, h.diagnosticSnapshot
	h.mu.Unlock()
	snapshot.addDestinationEvidence(replayRoot, journalSnapshot, diagnosticSnapshot)
	children := h.destinationMonitors()
	if len(children) > 0 {
		snapshot.Destinations = make(map[string]ExporterHealthSnapshot, len(children))
	}
	for name, child := range children {
		snapshot.Destinations[name] = child.Snapshot()
	}
	if len(snapshot.Destinations) > 0 {
		snapshot.Trace = aggregateDestinationSignal(snapshot.Destinations, false)
		snapshot.Metric = aggregateDestinationSignal(snapshot.Destinations, true)
	}
	return snapshot
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

func boundedExporterHealthExporter(exporter string) string {
	switch strings.TrimSpace(strings.ToLower(exporter)) {
	case exporterHealthExporterOTLP:
		return exporterHealthExporterOTLP
	case exporterHealthExporterAzureMonitor:
		return exporterHealthExporterAzureMonitor
	default:
		return exporterHealthExporterDefault
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
	} else if parsed, err := parseSchemeLessEndpoint(raw); err == nil && parsed.Host != "" {
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
	host = strings.TrimPrefix(host, "******")
	if host == "" {
		return "", "unknown"
	}
	if strings.ContainsAny(host, "/?#@\\") || strings.ContainsAny(host, " \t\r\n") {
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

func parseSchemeLessEndpoint(raw string) (*url.URL, error) {
	if strings.Contains(raw, "://") || !strings.ContainsAny(raw, "/?#") {
		return nil, errors.New("not a scheme-less endpoint")
	}
	return url.Parse("//" + raw)
}

// ExporterFailureReason returns the same bounded reason code stored in
// exporter-health state, without retaining the raw error text.
func ExporterFailureReason(err error) string {
	return exporterFailureReason(err)
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

func (h *ExporterHealth) pendingTransitionEventsLocked() []*journal.Event {
	events := make([]*journal.Event, 0, 4)
	events = append(events, h.pendingTransitionEventLocked("trace", &h.trace)...)
	events = append(events, h.pendingTransitionEventLocked("metric", &h.metric)...)
	return events
}

func (h *ExporterHealth) pendingTransitionEventLocked(signalName string, signal *exporterSignalHealth) []*journal.Event {
	events := make([]*journal.Event, 0, 3)
	if signal.failureTransitions > signal.journaledFailures {
		events = append(events, h.transitionEventLocked(signalName, exporterHealthStateUnhealthy, signal.lastFailureReason, signal.lastTransitionAt))
		signal.journaledFailures = signal.failureTransitions
	}
	if signal.pendingRefused != nil {
		events = append(events, signal.pendingRefused)
		signal.pendingRefused = nil
	}
	if signal.recoveryTransitions > signal.journaledRecoveries {
		events = append(events, h.transitionEventLocked(signalName, exporterHealthStateRecovered, signal.lastFailureReason, signal.lastTransitionAt))
		signal.journaledRecoveries = signal.recoveryTransitions
	}
	return events
}

func (h *ExporterHealth) transitionEventLocked(signalName, state, reason string, at time.Time) *journal.Event {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	runner := map[string]any{
		"annotation": exporterHealthTransitionAnnotation,
		"signal":     signalName,
		"state":      state,
		"mode":       h.mode,
	}
	if h.name != "" {
		runner["destination"] = h.name
	}
	if reason != "" {
		runner["reason"] = reason
	}
	if h.endpoint.class != "" {
		runner["endpointClass"] = h.endpoint.class
	}
	if h.endpoint.host != "" {
		runner["endpointHost"] = h.endpoint.host
	}
	return &journal.Event{
		Type:   journal.EventRunnerAnnotation,
		Time:   at,
		Reason: state,
		Runner: runner,
	}
}

func appendExporterHealthTransitions(log *journal.InstanceLog, events []*journal.Event) {
	for _, event := range events {
		appendExporterHealthTransition(log, event)
	}
}

func appendExporterHealthTransition(log *journal.InstanceLog, event *journal.Event) {
	if log == nil || event == nil {
		return
	}
	log.AppendBestEffort(*event)
}

type observedSpanExporter struct {
	next     sdktrace.SpanExporter
	health   *ExporterHealth
	exporter string
}

func (e observedSpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	err := e.next.ExportSpans(ctx, spans)
	if err != nil {
		e.health.recordTraceExporterFailure(e.exporter, err)
	} else {
		e.health.recordTraceExporterSuccess(e.exporter)
	}
	return err
}

func (e observedSpanExporter) Shutdown(ctx context.Context) error {
	err := e.next.Shutdown(ctx)
	if err != nil {
		e.health.recordTraceExporterFailure(e.exporter, err)
	} else if e.health == nil || e.health.name == "" {
		e.health.recordTraceExporterSuccess(e.exporter)
	}
	return err
}

type observedMetricExporter struct {
	next     metric.Exporter
	health   *ExporterHealth
	exporter string
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
		e.health.recordMetricExporterFailure(e.exporter, err)
	} else {
		e.health.recordMetricExporterSuccess(e.exporter)
	}
	return err
}

func (e observedMetricExporter) ForceFlush(ctx context.Context) error {
	err := e.next.ForceFlush(ctx)
	if err != nil {
		e.health.recordMetricExporterFailure(e.exporter, err)
	} else if e.health == nil || e.health.name == "" {
		e.health.recordMetricExporterSuccess(e.exporter)
	}
	return err
}

func (e observedMetricExporter) Shutdown(ctx context.Context) error {
	err := e.next.Shutdown(ctx)
	if err != nil {
		e.health.recordMetricExporterFailure(e.exporter, err)
	} else if e.health == nil || e.health.name == "" {
		e.health.recordMetricExporterSuccess(e.exporter)
	}
	return err
}
