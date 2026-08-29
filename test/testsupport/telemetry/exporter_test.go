package telemetry

import (
	"context"
	"testing"
)

func TestMemoryExporterReset(t *testing.T) {
	exporter := NewMemoryExporter()
	if err := exporter.ExportSpans(context.Background(), nil); err != nil {
		t.Fatalf("ExportSpans: %v", err)
	}
	exporter.Reset()
	if spans := exporter.Spans(); len(spans) != 0 {
		t.Fatalf("spans after Reset = %d, want 0", len(spans))
	}
	if err := exporter.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}
