package main

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/telemetry"
)

func TestEngineSpanSinkPassesAttemptIdentityToTaskSpan(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	client, err := telemetry.New(context.Background(), telemetry.Config{SpanExporter: exporter})
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	sink := engineSpanSink{client: client}
	runID := "0123456789abcdef0123456789abcdef"
	runCtx, runSpan, err := sink.StartRunSpan(context.Background(), engine.RunSpanID{
		Gaggle: "web", WorkflowID: "implement", RunID: runID,
	}, time.Now())
	if err != nil {
		t.Fatalf("StartRunSpan: %v", err)
	}
	_, stageSpan, err := sink.StartStageSpan(runCtx, engine.StageSpanID{
		RunSpanID: engine.RunSpanID{Gaggle: "web", WorkflowID: "implement", RunID: runID},
		Stage:     "implement", Attempt: 2, BuildID: "build-7", WorkerIdentity: "worker-7",
	}, time.Now())
	if err != nil {
		t.Fatalf("StartStageSpan: %v", err)
	}
	stageSpan.Complete("success", false)
	stageSpan.EndAt(time.Now())
	runSpan.EndAt(time.Now())
	if err := client.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	spans := exporter.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("exported spans = %d, want run and task", len(spans))
	}
	var attrs map[string]string
	for _, span := range spans {
		if span.Name == "task/implement" {
			attrs = map[string]string{}
			for _, attr := range span.Attributes {
				attrs[string(attr.Key)] = attr.Value.AsString()
			}
		}
	}
	if attrs == nil {
		t.Fatal("task span was not exported")
	}
	if attrs[telemetry.AttrBuildID] != "build-7" || attrs[telemetry.AttrWorkerIdentity] != "worker-7" {
		t.Fatalf("task identity attributes = %v, want build-7/worker-7", attrs)
	}
}
