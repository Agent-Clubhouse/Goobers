package telemetry

import (
	"context"
	"sync"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type MemoryExporter struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

func NewMemoryExporter() *MemoryExporter {
	return &MemoryExporter{}
}

func (e *MemoryExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, spans...)
	return nil
}

func (e *MemoryExporter) Shutdown(context.Context) error {
	return nil
}

func (e *MemoryExporter) Spans() []sdktrace.ReadOnlySpan {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]sdktrace.ReadOnlySpan(nil), e.spans...)
}

func (e *MemoryExporter) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = nil
}
