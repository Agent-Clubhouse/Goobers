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
)

// JournalSpanExporter is an OpenTelemetry sdktrace.SpanExporter that writes
// completed spans under a run's journal directory: runs/<traceID>/spans/spans.jsonl.
// A Goobers run IS an OTel trace (NewRunID mints the trace id used as the run
// id), so grouping exported spans by trace id is exactly grouping them by run.
// Spans are telemetry, not the conformance-normative journal (§3.3, §4) — this
// exporter never touches events.jsonl/run.yaml/state.json.
type JournalSpanExporter struct {
	runsDir       string
	perGaggleRoot string
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

// NewPerGaggleJournalSpanExporter creates an exporter that routes each span to
// <instance-root>/gaggles/<gaggle>/runs using its goobers.gaggle attribute.
func NewPerGaggleJournalSpanExporter(instanceRoot string, scrubber journal.Scrubber) *JournalSpanExporter {
	exporter := NewJournalSpanExporter("", scrubber)
	exporter.perGaggleRoot = instanceRoot
	return exporter
}

// ExportSpans writes each span as one JSON line under its run's spans/
// directory, grouping the batch by trace id so one call may fan out across
// several concurrent runs. Attribute and event values are redacted before
// write (TEL-013 defense in depth).
func (e *JournalSpanExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	byTrace := make(map[string][]sdktrace.ReadOnlySpan, len(spans))
	for _, s := range spans {
		traceID := s.SpanContext().TraceID().String()
		byTrace[traceID] = append(byTrace[traceID], s)
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
	}
	dir := filepath.Join(runsDir, traceID, spansDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("telemetry: create spans dir for run %s: %w", traceID, err)
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
			return fmt.Errorf("telemetry: encode span for run %s: %w", traceID, err)
		}
	}
	return f.Sync()
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
