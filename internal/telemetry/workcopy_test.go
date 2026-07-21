package telemetry

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/codes"

	"github.com/goobers/goobers/internal/worktree"
)

func TestRecordWorkcopyUsageAttributesCurrentRunSpan(t *testing.T) {
	exporter := NewMemoryExporter()
	client, err := New(context.Background(), Config{ServiceName: "workcopy-usage-test", SpanExporter: exporter})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })
	runID, err := NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	ctx, span, err := client.StartTask(context.Background(), TaskAttributes{
		Gaggle: "alpha", WorkflowID: "implementation", RunID: runID, TaskID: "implement",
	})
	if err != nil {
		t.Fatal(err)
	}

	client.RecordWorkcopyUsage(ctx, worktree.UsageMeasurement{
		Operation:        worktree.UsageOperationCreate,
		Gaggle:           "alpha",
		OwnerRunID:       runID,
		WorktreeID:       runID + "-implement",
		WorktreeBytes:    128,
		WorktreeMeasured: true,
		WorkcopyBytes:    1024,
		WorkcopyMeasured: true,
	})
	span.End()

	events := exporter.Spans()[0].Events()
	if len(events) != 2 {
		t.Fatalf("events = %#v, want worktree and aggregate usage", events)
	}
	if events[0].Name != EventWorktreeDiskUsage || events[1].Name != EventWorkcopyDiskUsage {
		t.Fatalf("event names = %q, %q", events[0].Name, events[1].Name)
	}
	assertSpanEventAttribute(t, events[0].Attributes, AttrRunID, runID)
	assertSpanEventAttribute(t, events[0].Attributes, AttrGaggle, "alpha")
	assertSpanEventAttribute(t, events[0].Attributes, AttrWorktreeID, runID+"-implement")
	assertSpanEventAttribute(t, events[0].Attributes, metricValueAttribute, "128")
	assertSpanEventAttribute(t, events[1].Attributes, metricValueAttribute, "1024")
}

func TestRecordWorkcopyUsageSurfacesStandaloneHousekeepingFailure(t *testing.T) {
	exporter := NewMemoryExporter()
	client, err := New(context.Background(), Config{ServiceName: "workcopy-failure-test", SpanExporter: exporter})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	client.RecordWorkcopyUsage(context.Background(), worktree.UsageMeasurement{
		Operation: worktree.UsageOperationHousekeeping,
		Gaggle:    "alpha",
		Err:       errors.New("fixture disk failure"),
	})

	spans := exporter.Spans()
	if len(spans) != 1 || spans[0].Name() != "scheduler/workcopy-housekeeping" {
		t.Fatalf("standalone spans = %#v", spans)
	}
	if spans[0].Status().Code != codes.Error {
		t.Fatalf("span status = %s, want Error", spans[0].Status().Code)
	}
	var failureFound bool
	for _, event := range spans[0].Events() {
		if event.Name != EventWorkcopyDiskMeasurementFailed {
			continue
		}
		failureFound = true
		assertSpanEventAttribute(t, event.Attributes, AttrErrorMessage, "fixture disk failure")
	}
	if !failureFound {
		t.Fatalf("measurement failure event missing: %#v", spans[0].Events())
	}
}
