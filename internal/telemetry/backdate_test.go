package telemetry

import (
	"context"
	"testing"
	"time"
)

// A tier-3 run's spans are synthesized after the fact, from workflow history via
// the journal projection. A span stamped at synthesis time would report the
// wrong moment and a duration measured against the projection rather than the
// run, which is worse than no telemetry: it looks authoritative and is wrong.
func TestStartRunBackdatesWhenStartedAtIsSet(t *testing.T) {
	t.Parallel()
	exporter := NewMemoryExporter()
	client, err := New(context.Background(), Config{SpanExporter: exporter})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	want := time.Date(2026, 8, 12, 11, 42, 3, 0, time.UTC)
	_, span, err := client.StartRun(context.Background(), RunAttributes{
		Gaggle:     "testbed-minimal",
		WorkflowID: "cross-platform-implement",
		RunID:      "9740bc5d00465885af6fa5b1b263c61e",
		StartedAt:  want,
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	span.End()
	if err := client.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	spans := exporter.Spans()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans, want 1", len(spans))
	}
	if got := spans[0].StartTime().UTC(); !got.Equal(want) {
		t.Fatalf("StartTime = %s, want %s — the span was stamped at creation rather than backdated", got, want)
	}
}

// The live tier-1 path passes no StartedAt, and must keep stamping at creation.
// Backdating is opt-in or it is a behaviour change to every existing caller.
func TestStartRunStampsNowWhenStartedAtIsZero(t *testing.T) {
	t.Parallel()
	exporter := NewMemoryExporter()
	client, err := New(context.Background(), Config{SpanExporter: exporter})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	before := time.Now()
	_, span, err := client.StartRun(context.Background(), RunAttributes{
		Gaggle:     "testbed-minimal",
		WorkflowID: "default-implement",
		RunID:      "300534f6f9503e251374d9433060ebf8",
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	span.End()
	if err := client.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	spans := exporter.Spans()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans, want 1", len(spans))
	}
	if got := spans[0].StartTime(); got.Before(before) {
		t.Fatalf("StartTime = %s, want >= %s — a zero StartedAt must not backdate", got, before)
	}
}
