// Package telemetry provides test support for telemetry producers.
package telemetry

import (
	"context"
	"sync"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// MemoryExporter records exported spans in memory.
type MemoryExporter struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

// NewMemoryExporter returns an empty in-memory span exporter.
func NewMemoryExporter() *MemoryExporter {
	return &MemoryExporter{}
}

// ExportSpans records spans for later inspection.
func (e *MemoryExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, spans...)
	return nil
}

// Shutdown releases exporter resources.
func (e *MemoryExporter) Shutdown(context.Context) error {
	return nil
}

// Spans returns a copy of the recorded spans.
func (e *MemoryExporter) Spans() []sdktrace.ReadOnlySpan {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]sdktrace.ReadOnlySpan(nil), e.spans...)
}

// Reset clears the recorded spans.
func (e *MemoryExporter) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = nil
}
