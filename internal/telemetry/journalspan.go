package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/goobers/goobers/internal/journal"
)

// spansDirName and spanFileName match the run journal's reserved spans/ layout
// (ARCHITECTURE.md §4; internal/journal's dirSpans + fileEvents naming
// convention) without importing internal/journal — issue #22 builds against
// the documented on-disk shape, decoupled from #8's still-unmerged package
// (same playbook as #12's provider seams).
const (
	spansDirName = "spans"
	spanFileName = "spans.jsonl"
	otlpFileName = "otlp.jsonl"
)

// OTLPJSONContentType and OTLPJSONMessageType define the framing of
// spans/otlp.jsonl: newline-delimited OTLP/JSON, one v1 export request per
// line.
const (
	OTLPJSONContentType = "application/x-ndjson"
	OTLPJSONMessageType = "opentelemetry.proto.collector.trace.v1.ExportTraceServiceRequest"
)

// JournalSpanExporter is an OpenTelemetry sdktrace.SpanExporter that writes
// completed run spans under runs/<traceID>/spans/spans.jsonl and, when
// instance-scoped, scheduler spans under scheduler/spans/spans.jsonl. Spans are
// telemetry, not the conformance-normative journal (§3.3, §4).
type JournalSpanExporter struct {
	runsDir       string
	perGaggleRoot string
	schedulerDir  string
	scrubber      journal.Scrubber
}

// NewJournalSpanExporter creates an exporter that writes spans under runsDir
// (the instance's runs/ directory), redacting attribute/status values through
// scrubber before write. Pass a Chain(registry, PatternScrubber) so
// resolver-issued secrets registered for a run are caught in that run's spans,
// not only pattern-shaped ones (#117 Piece B). A nil scrubber defaults to the
// shared pattern net (pattern-only, no registry). runsDir is created if missing.
func NewJournalSpanExporter(runsDir string, scrubber journal.Scrubber) *JournalSpanExporter {
	if scrubber == nil {
		scrubber = journal.NewPatternScrubber()
	}
	return &JournalSpanExporter{runsDir: runsDir, scrubber: scrubber}
}

// NewPerGaggleJournalSpanExporter creates an exporter that routes run spans to
// <instance-root>/gaggles/<gaggle>/runs using their goobers.gaggle attribute,
// except when the run already has a retained flat journal under runs/. It
// routes scheduler spans to the instance-level scheduler directory.
func NewPerGaggleJournalSpanExporter(instanceRoot string, scrubber journal.Scrubber) *JournalSpanExporter {
	exporter := NewJournalSpanExporter("", scrubber)
	exporter.perGaggleRoot = instanceRoot
	exporter.schedulerDir = filepath.Join(instanceRoot, "scheduler")
	return exporter
}

// ExportSpans writes run spans under their run directories and scheduler spans
// to the instance-level scheduler/spans/spans.jsonl file. Attribute and event
// values are redacted before write (TEL-013 defense in depth).
func (e *JournalSpanExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	byTrace := make(map[string][]sdktrace.ReadOnlySpan, len(spans))
	var schedulerSpans []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if e.schedulerDir != "" && spanRecordKind(s.Name()) == SpanKindScheduler {
			schedulerSpans = append(schedulerSpans, s)
			continue
		}
		traceID := s.SpanContext().TraceID().String()
		byTrace[traceID] = append(byTrace[traceID], s)
	}
	if len(schedulerSpans) > 0 {
		if err := e.writeSpans(filepath.Join(e.schedulerDir, spansDirName), "scheduler", schedulerSpans); err != nil {
			return err
		}
	}
	for traceID, group := range byTrace {
		if err := e.writeGroup(traceID, group); err != nil {
			return err
		}
	}
	return nil
}

// Shutdown is a no-op: every ExportSpans call already flushes and fsyncs.
func (e *JournalSpanExporter) Shutdown(context.Context) error { return nil }

func (e *JournalSpanExporter) writeGroup(traceID string, spans []sdktrace.ReadOnlySpan) error {
	runsDir := e.runsDir
	if e.perGaggleRoot != "" {
		gaggle, err := spanGaggle(spans)
		if err != nil {
			return fmt.Errorf("telemetry: resolve gaggle for run %s: %w", traceID, err)
		}
		runsDir = filepath.Join(e.perGaggleRoot, "gaggles", gaggle, "runs")
		legacyRunsDir := filepath.Join(e.perGaggleRoot, "runs")
		if info, err := os.Stat(filepath.Join(legacyRunsDir, traceID, "run.yaml")); err == nil && info.Mode().IsRegular() {
			runsDir = legacyRunsDir
		} else if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("telemetry: inspect retained journal for run %s: %w", traceID, err)
		}
	}
	dir := filepath.Join(runsDir, traceID, spansDirName)
	otlpRecord, err := e.marshalOTLP(spans)
	if err != nil {
		return fmt.Errorf("telemetry: encode OTLP spans for run %s: %w", traceID, err)
	}

	if err := e.writeSpans(dir, "run "+traceID, spans); err != nil {
		return err
	}

	return e.writeOTLP(filepath.Join(dir, otlpFileName), otlpRecord)
}

func (e *JournalSpanExporter) writeSpans(dir, owner string, spans []sdktrace.ReadOnlySpan) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("telemetry: create spans dir for %s: %w", owner, err)
	}
	path := filepath.Join(dir, spanFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("telemetry: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	enc := json.NewEncoder(f)
	for _, s := range spans {
		if err := enc.Encode(e.toSpanRecord(s)); err != nil {
			return fmt.Errorf("telemetry: encode span for %s: %w", owner, err)
		}
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("telemetry: sync %s: %w", path, err)
	}
	return nil
}

func (e *JournalSpanExporter) writeOTLP(path string, record []byte) error {
	otlp, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("telemetry: open %s: %w", path, err)
	}
	defer func() { _ = otlp.Close() }()

	if _, err := otlp.Write(append(record, '\n')); err != nil {
		return fmt.Errorf("telemetry: append %s: %w", path, err)
	}
	if err := otlp.Sync(); err != nil {
		return fmt.Errorf("telemetry: sync %s: %w", path, err)
	}
	return nil
}

func spanGaggle(spans []sdktrace.ReadOnlySpan) (string, error) {
	var gaggle string
	for _, span := range spans {
		for _, attr := range span.Attributes() {
			if string(attr.Key) != AttrGaggle {
				continue
			}
			value := attr.Value.AsString()
			if value == "" || value == "." || value == ".." || filepath.Base(value) != value {
				return "", fmt.Errorf("invalid gaggle attribute %q", value)
			}
			if gaggle != "" && gaggle != value {
				return "", fmt.Errorf("trace contains spans from gaggles %q and %q", gaggle, value)
			}
			gaggle = value
		}
	}
	if gaggle == "" {
		return "", fmt.Errorf("missing %s attribute", AttrGaggle)
	}
	return gaggle, nil
}

// SpanRecord is the on-disk shape of one line in spans/spans.jsonl. Field names
// are stable — the rollup ingester (internal/telemetry/rollup) decodes this
// exact shape.
type SpanRecord struct {
	Schema        string            `json:"schema"`
	TraceID       string            `json:"traceId"`
	SpanID        string            `json:"spanId"`
	ParentSpanID  string            `json:"parentSpanId,omitempty"`
	Name          string            `json:"name"`
	Kind          string            `json:"kind,omitempty"` // run|task|gate|scheduler
	StartTime     time.Time         `json:"startTime"`
	EndTime       time.Time         `json:"endTime"`
	Status        string            `json:"status"` // ok|error|unset
	StatusMessage string            `json:"statusMessage,omitempty"`
	Attributes    map[string]string `json:"attributes,omitempty"`
	Events        []SpanEventRecord `json:"events,omitempty"`
	DroppedEvents int               `json:"droppedEvents,omitempty"`
}

// SpanEventRecord is a within-stage harness event attached to its stage span
// (TEL-012), recorded via Span.Event.
type SpanEventRecord struct {
	Name              string            `json:"name"`
	Time              time.Time         `json:"time"`
	Attributes        map[string]string `json:"attributes,omitempty"`
	DroppedAttributes int               `json:"droppedAttributes,omitempty"`
}

// SpanSchema is the schema id stamped on every span record, versioned
// independently of the journal event schema since spans are excluded from
// conformance (§3.3) and may evolve on their own cadence.
const SpanSchema = "goobers.dev/telemetry/span/v1"

func (e *JournalSpanExporter) toSpanRecord(s sdktrace.ReadOnlySpan) SpanRecord {
	sc := s.SpanContext()
	rec := SpanRecord{
		Schema:        SpanSchema,
		TraceID:       sc.TraceID().String(),
		SpanID:        sc.SpanID().String(),
		Name:          redactWith(e.scrubber, s.Name()),
		StartTime:     s.StartTime(),
		EndTime:       s.EndTime(),
		Status:        statusString(s.Status().Code),
		Attributes:    e.stringifyAttrs(s.Attributes()),
		DroppedEvents: s.DroppedEvents(),
	}
	if parent := s.Parent(); parent.HasSpanID() {
		rec.ParentSpanID = parent.SpanID().String()
	}
	rec.Kind = spanRecordKind(rec.Name)
	if desc := s.Status().Description; desc != "" {
		rec.StatusMessage = redactWith(e.scrubber, desc)
	}
	for _, ev := range s.Events() {
		rec.Events = append(rec.Events, SpanEventRecord{
			Name:              redactWith(e.scrubber, ev.Name),
			Time:              ev.Time,
			Attributes:        e.stringifyAttrs(ev.Attributes),
			DroppedAttributes: ev.DroppedAttributeCount,
		})
	}
	return rec
}

func spanRecordKind(name string) string {
	switch {
	case strings.HasPrefix(name, "run/"):
		return SpanKindRun
	case strings.HasPrefix(name, "task/"):
		return SpanKindTask
	case strings.HasPrefix(name, "gate/"):
		return SpanKindGate
	case strings.HasPrefix(name, "scheduler/"):
		return SpanKindScheduler
	default:
		return ""
	}
}

func statusString(code codes.Code) string {
	switch code {
	case codes.Error:
		return "error"
	case codes.Ok:
		return "ok"
	default:
		return "unset"
	}
}

func (e *JournalSpanExporter) stringifyAttrs(kvs []attribute.KeyValue) map[string]string {
	if len(kvs) == 0 {
		return nil
	}
	out := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		out[redactWith(e.scrubber, string(kv.Key))] = redactWith(e.scrubber, kv.Value.Emit())
	}
	return out
}
