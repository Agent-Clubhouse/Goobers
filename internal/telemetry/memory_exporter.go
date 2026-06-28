package telemetry

import (
	"context"
	"sync"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// MemoryExporter stores completed spans for tests and local assertions.
type MemoryExporter struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

// NewMemoryExporter creates an in-memory span exporter for tests.
func NewMemoryExporter() *MemoryExporter {
	return &MemoryExporter{}
}

// ExportSpans stores exported spans in memory.
func (e *MemoryExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, spans...)
	return nil
}

// Shutdown is a no-op for the in-memory exporter.
func (e *MemoryExporter) Shutdown(context.Context) error {
	return nil
}

// Spans returns a snapshot of exported spans.
func (e *MemoryExporter) Spans() []sdktrace.ReadOnlySpan {
	e.mu.Lock()
	defer e.mu.Unlock()
	spans := make([]sdktrace.ReadOnlySpan, len(e.spans))
	copy(spans, e.spans)
	return spans
}

// Reset clears exported spans.
func (e *MemoryExporter) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = nil
}
