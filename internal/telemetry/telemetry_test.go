package telemetry

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func TestRunTaskGateSpansUseRunTraceAndAttributes(t *testing.T) {
	ctx := context.Background()
	exporter := NewMemoryExporter()
	client, err := New(ctx, Config{
		ServiceName:  "telemetry-test",
		SpanExporter: exporter,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := client.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	})

	runID := "0af7651916cd43dd8448eb211c80319c"
	runCtx, runSpan, err := client.StartRun(ctx, RunAttributes{
		Gaggle:          "acme-web",
		WorkflowID:      "default-implement",
		WorkflowVersion: "v7",
		RunID:           runID,
		ItemID:          "42",
		ItemProvider:    "github",
		Trigger:         "backlog",
	})
	if err != nil {
		t.Fatalf("StartRun() error = %v", err)
	}

	_, taskSpan, err := client.StartTask(runCtx, TaskAttributes{
		Gaggle:     "acme-web",
		WorkflowID: "default-implement",
		RunID:      runID,
		TaskID:     "implement",
		TaskType:   "agentic",
		GooberID:   "coder",
	})
	if err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}
	taskSpan.Event("tool.completed", attribute.String("tool.name", "go-test"))
	taskSpan.Succeed("task completed")

	_, gateSpan, err := client.StartGate(runCtx, GateAttributes{
		Gaggle:     "acme-web",
		WorkflowID: "default-implement",
		RunID:      runID,
		GateID:     "qa",
		Evaluator:  "agentic",
		Decision:   "pass",
		GooberID:   "reviewer",
	})
	if err != nil {
		t.Fatalf("StartGate() error = %v", err)
	}
	gateSpan.End()
	runSpan.End()

	if err := client.Flush(ctx); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}

	spans := exporter.Spans()
	if len(spans) != 3 {
		t.Fatalf("expected 3 spans, got %d", len(spans))
	}
	run := findSpan(t, spans, "run/default-implement")
	task := findSpan(t, spans, "task/implement")
	gate := findSpan(t, spans, "gate/qa")

	expectedTraceID, err := trace.TraceIDFromHex(runID)
	if err != nil {
		t.Fatalf("TraceIDFromHex() error = %v", err)
	}
	for _, span := range []sdktrace.ReadOnlySpan{run, task, gate} {
		if span.SpanContext().TraceID() != expectedTraceID {
			t.Fatalf("%s trace id = %s, want %s", span.Name(), span.SpanContext().TraceID(), expectedTraceID)
		}
	}
	if run.Parent().IsValid() {
		t.Fatalf("run parent = %s, want root span", run.Parent().SpanID())
	}
	if task.Parent().SpanID() != run.SpanContext().SpanID() {
		t.Fatalf("task parent = %s, want run span %s", task.Parent().SpanID(), run.SpanContext().SpanID())
	}
	if gate.Parent().SpanID() != run.SpanContext().SpanID() {
		t.Fatalf("gate parent = %s, want run span %s", gate.Parent().SpanID(), run.SpanContext().SpanID())
	}

	runAttrs := attrMap(run)
	assertAttr(t, runAttrs, AttrSpanKind, SpanKindRun)
	assertAttr(t, runAttrs, AttrGaggle, "acme-web")
	assertAttr(t, runAttrs, AttrWorkflowID, "default-implement")
	assertAttr(t, runAttrs, AttrRunID, runID)
	assertAttr(t, runAttrs, AttrItemID, "42")
	assertAttr(t, runAttrs, AttrTrigger, "backlog")

	taskAttrs := attrMap(task)
	assertAttr(t, taskAttrs, AttrSpanKind, SpanKindTask)
	assertAttr(t, taskAttrs, AttrTaskID, "implement")
	assertAttr(t, taskAttrs, AttrTaskType, "agentic")
	assertAttr(t, taskAttrs, AttrGooberID, "coder")
	if task.Status().Code != codes.Ok {
		t.Fatalf("task status = %s, want OK", task.Status().Code)
	}
	if len(task.Events()) != 1 || task.Events()[0].Name != "tool.completed" {
		t.Fatalf("task events = %#v, want tool.completed event", task.Events())
	}

	gateAttrs := attrMap(gate)
	assertAttr(t, gateAttrs, AttrSpanKind, SpanKindGate)
	assertAttr(t, gateAttrs, AttrGateID, "qa")
	assertAttr(t, gateAttrs, AttrGateEvaluator, "agentic")
	assertAttr(t, gateAttrs, AttrGateDecision, "pass")
}

func TestSpanEventLimitBoundsRetryAccumulation(t *testing.T) {
	exporter := NewMemoryExporter()
	client, err := New(context.Background(), Config{ServiceName: "telemetry-limit-test", SpanExporter: exporter})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })
	runID, err := NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	_, span, err := client.StartTask(context.Background(), TaskAttributes{
		Gaggle: "web", WorkflowID: "wf", RunID: runID, TaskID: "retrying",
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxSpanEvents+7; i++ {
		span.Event("attempt.event")
	}
	span.End()

	got := exporter.Spans()[0]
	if len(got.Events()) != maxSpanEvents {
		t.Fatalf("retained events = %d, want %d", len(got.Events()), maxSpanEvents)
	}
	if got.DroppedEvents() != 7 {
		t.Fatalf("dropped events = %d, want 7", got.DroppedEvents())
	}
	record := NewJournalSpanExporter(t.TempDir(), nil).toSpanRecord(got)
	if record.DroppedEvents != 7 {
		t.Fatalf("journal dropped events = %d, want 7", record.DroppedEvents)
	}
}

func TestSchedulerSpanAttributes(t *testing.T) {
	ctx := context.Background()
	exporter := NewMemoryExporter()
	client, err := New(ctx, Config{ServiceName: "telemetry-test", SpanExporter: exporter})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := client.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	})

	_, span, err := client.StartSchedulerSpan(ctx, SchedulerAttributes{
		Gaggle:       "acme-web",
		WorkflowID:   "default-implement",
		Action:       "claim",
		Reason:       "capacity-available",
		ItemID:       "42",
		ItemProvider: "ado",
	})
	if err != nil {
		t.Fatalf("StartSchedulerSpan() error = %v", err)
	}
	span.End()

	scheduler := findSpan(t, exporter.Spans(), "scheduler/claim")
	attrs := attrMap(scheduler)
	assertAttr(t, attrs, AttrSpanKind, SpanKindScheduler)
	assertAttr(t, attrs, AttrSchedulerAction, "claim")
	assertAttr(t, attrs, AttrSchedulerReason, "capacity-available")
	assertAttr(t, attrs, AttrItemProvider, "ado")
}

func TestSchedulerSpanCanUseRunTraceIDWithoutParentContext(t *testing.T) {
	ctx := context.Background()
	exporter := NewMemoryExporter()
	client, err := New(ctx, Config{ServiceName: "telemetry-test", SpanExporter: exporter})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := client.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	})

	runID, err := NewRunID()
	if err != nil {
		t.Fatalf("NewRunID() error = %v", err)
	}
	_, span, err := client.StartSchedulerSpan(ctx, SchedulerAttributes{
		Gaggle:     "acme-web",
		WorkflowID: "default-implement",
		RunID:      runID,
		Action:     "start",
	})
	if err != nil {
		t.Fatalf("StartSchedulerSpan() error = %v", err)
	}
	span.End()

	scheduler := findSpan(t, exporter.Spans(), "scheduler/start")
	expectedTraceID, err := trace.TraceIDFromHex(runID)
	if err != nil {
		t.Fatalf("TraceIDFromHex() error = %v", err)
	}
	if scheduler.SpanContext().TraceID() != expectedTraceID {
		t.Fatalf("scheduler trace id = %s, want %s", scheduler.SpanContext().TraceID(), expectedTraceID)
	}
}

func TestStdoutExporterWritesLocalSpans(t *testing.T) {
	ctx := context.Background()
	var out bytes.Buffer
	client, err := New(ctx, Config{
		ServiceName:        "telemetry-test",
		ServiceVersion:     "v1",
		Environment:        "test",
		Exporter:           ExporterStdout,
		Stdout:             &out,
		ResourceAttributes: []attribute.KeyValue{attribute.String("instance.id", "local")},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := client.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	})

	_, span, err := client.StartRun(ctx, RunAttributes{
		Gaggle:     "acme-web",
		WorkflowID: "wf",
		RunID:      "0af7651916cd43dd8448eb211c80319c",
	})
	if err != nil {
		t.Fatalf("StartRun() error = %v", err)
	}
	span.End()
	if err := client.Flush(ctx); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	if !strings.Contains(out.String(), "run/wf") {
		t.Fatalf("stdout exporter output = %q, want run/wf", out.String())
	}
}

func TestMemoryExporterReset(t *testing.T) {
	exporter := NewMemoryExporter()
	client, err := New(context.Background(), Config{ServiceName: "telemetry-test", SpanExporter: exporter})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := client.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	})
	_, span, err := client.StartRun(context.Background(), RunAttributes{
		Gaggle:     "acme-web",
		WorkflowID: "wf",
		RunID:      "0af7651916cd43dd8448eb211c80319c",
	})
	if err != nil {
		t.Fatalf("StartRun() error = %v", err)
	}
	span.End()
	if len(exporter.Spans()) != 1 {
		t.Fatalf("spans before reset = %d, want 1", len(exporter.Spans()))
	}
	exporter.Reset()
	if len(exporter.Spans()) != 0 {
		t.Fatalf("spans after reset = %d, want 0", len(exporter.Spans()))
	}
}

func TestInvalidRunIDIsRejected(t *testing.T) {
	ctx := context.Background()
	client, err := New(ctx, Config{ServiceName: "telemetry-test", SpanExporter: NewMemoryExporter()})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := client.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	})

	_, _, err = client.StartRun(ctx, RunAttributes{
		Gaggle:     "acme-web",
		WorkflowID: "default-implement",
		RunID:      "not-a-trace-id",
	})
	if err == nil {
		t.Fatal("StartRun() error = nil, want invalid run id error")
	}
}

func TestMismatchedParentTraceIsRejected(t *testing.T) {
	ctx := context.Background()
	client, err := New(ctx, Config{ServiceName: "telemetry-test", SpanExporter: NewMemoryExporter()})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := client.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	})

	runCtx, runSpan, err := client.StartRun(ctx, RunAttributes{
		Gaggle:     "acme-web",
		WorkflowID: "wf",
		RunID:      "0af7651916cd43dd8448eb211c80319c",
	})
	if err != nil {
		t.Fatalf("StartRun() error = %v", err)
	}
	defer runSpan.End()

	_, _, err = client.StartTask(runCtx, TaskAttributes{
		Gaggle:     "acme-web",
		WorkflowID: "wf",
		RunID:      "1af7651916cd43dd8448eb211c80319c",
		TaskID:     "implement",
	})
	if err == nil {
		t.Fatal("StartTask() error = nil, want mismatched parent trace error")
	}
}

func TestValidationErrorsAreReturned(t *testing.T) {
	ctx := context.Background()
	client, err := New(ctx, Config{ServiceName: "telemetry-test", SpanExporter: NewMemoryExporter()})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := client.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	})

	runID := "0af7651916cd43dd8448eb211c80319c"
	cases := []struct {
		name string
		fn   func() error
	}{
		{
			name: "missing gaggle",
			fn: func() error {
				_, _, err := client.StartRun(ctx, RunAttributes{WorkflowID: "wf", RunID: runID})
				return err
			},
		},
		{
			name: "missing task id",
			fn: func() error {
				_, _, err := client.StartTask(ctx, TaskAttributes{Gaggle: "acme-web", WorkflowID: "wf", RunID: runID})
				return err
			},
		},
		{
			name: "missing gate id",
			fn: func() error {
				_, _, err := client.StartGate(ctx, GateAttributes{Gaggle: "acme-web", WorkflowID: "wf", RunID: runID})
				return err
			},
		},
		{
			name: "missing scheduler action",
			fn: func() error {
				_, _, err := client.StartSchedulerSpan(ctx, SchedulerAttributes{Gaggle: "acme-web", WorkflowID: "wf"})
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.fn(); err == nil {
				t.Fatal("error = nil, want validation error")
			}
		})
	}
}

func TestUnsupportedExporterIsRejected(t *testing.T) {
	_, err := New(context.Background(), Config{ServiceName: "telemetry-test", Exporter: ExporterKind("bogus")})
	if err == nil {
		t.Fatal("New() error = nil, want unsupported exporter error")
	}
}

func TestSpanFailRecordsErrorStatus(t *testing.T) {
	ctx := context.Background()
	exporter := NewMemoryExporter()
	client, err := New(ctx, Config{ServiceName: "telemetry-test", SpanExporter: exporter})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := client.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	})

	runID := "0af7651916cd43dd8448eb211c80319c"
	runCtx, runSpan, err := client.StartRun(ctx, RunAttributes{Gaggle: "acme-web", WorkflowID: "wf", RunID: runID})
	if err != nil {
		t.Fatalf("StartRun() error = %v", err)
	}
	_, taskSpan, err := client.StartTask(runCtx, TaskAttributes{Gaggle: "acme-web", WorkflowID: "wf", RunID: runID, TaskID: "broken"})
	if err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}
	taskSpan.Fail(errors.New("boom"))
	runSpan.End()

	task := findSpan(t, exporter.Spans(), "task/broken")
	if task.Status().Code != codes.Error {
		t.Fatalf("task status = %s, want Error", task.Status().Code)
	}
	if task.Status().Description != "boom" {
		t.Fatalf("task status description = %q, want boom", task.Status().Description)
	}
}

// TestSpanCompleteRecordsBusinessStatus is issue #710's span-fix acceptance:
// a business failure (isFailure=true) sets OTel status codes.Error — the
// prior span.Succeed(status) call reported codes.Ok for a failed run/stage
// span, so `goobers trace`/rollup span queries couldn't distinguish a failed
// run from a healthy one without reading free-text. Complete's second
// property — the goobers.business_status attribute — is what an ok/success
// business status ALSO needs recorded (not just the failure path), so a
// rollup consumer can query the actual outcome vocabulary
// (success/failed/completed/escalated/aborted) independent of OTel's own
// coarser two-value axis.
func TestSpanCompleteRecordsBusinessStatus(t *testing.T) {
	ctx := context.Background()
	exporter := NewMemoryExporter()
	client, err := New(ctx, Config{ServiceName: "telemetry-test", SpanExporter: exporter})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := client.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	})

	runID := "0af7651916cd43dd8448eb211c80319c"
	runCtx, runSpan, err := client.StartRun(ctx, RunAttributes{Gaggle: "acme-web", WorkflowID: "wf", RunID: runID})
	if err != nil {
		t.Fatalf("StartRun() error = %v", err)
	}
	_, failedTask, err := client.StartTask(runCtx, TaskAttributes{Gaggle: "acme-web", WorkflowID: "wf", RunID: runID, TaskID: "pr-select"})
	if err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}
	failedTask.Complete("failure", true)

	_, okTask, err := client.StartTask(runCtx, TaskAttributes{Gaggle: "acme-web", WorkflowID: "wf", RunID: runID, TaskID: "implement"})
	if err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}
	okTask.Complete("success", false)
	runSpan.Complete("failed", true)

	spans := exporter.Spans()
	failed := findSpan(t, spans, "task/pr-select")
	if failed.Status().Code != codes.Error {
		t.Fatalf("failed task status = %s, want Error — a business failure must not report codes.Ok (#705's root cause)", failed.Status().Code)
	}
	if failed.Status().Description != "failure" {
		t.Fatalf("failed task status description = %q, want failure", failed.Status().Description)
	}
	if got := attrMap(failed)[AttrBusinessStatus]; got != "failure" {
		t.Fatalf("failed task %s attribute = %q, want failure", AttrBusinessStatus, got)
	}

	ok := findSpan(t, spans, "task/implement")
	if ok.Status().Code != codes.Ok {
		t.Fatalf("ok task status = %s, want Ok", ok.Status().Code)
	}
	if got := attrMap(ok)[AttrBusinessStatus]; got != "success" {
		t.Fatalf("ok task %s attribute = %q, want success", AttrBusinessStatus, got)
	}

	run := findSpan(t, spans, "run/wf")
	if run.Status().Code != codes.Error {
		t.Fatalf("run status = %s, want Error — the run's OWN terminal phase was failed", run.Status().Code)
	}
	if got := attrMap(run)[AttrBusinessStatus]; got != "failed" {
		t.Fatalf("run %s attribute = %q, want failed", AttrBusinessStatus, got)
	}
}

func findSpan(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, span := range spans {
		if span.Name() == name {
			return span
		}
	}
	t.Fatalf("span %q not found in %v", name, spanNames(spans))
	return nil
}

func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	names := make([]string, 0, len(spans))
	for _, span := range spans {
		names = append(names, span.Name())
	}
	return names
}

func attrMap(span sdktrace.ReadOnlySpan) map[string]string {
	attrs := map[string]string{}
	for _, attr := range span.Attributes() {
		attrs[string(attr.Key)] = attr.Value.AsString()
	}
	return attrs
}

func assertAttr(t *testing.T, attrs map[string]string, key, want string) {
	t.Helper()
	if got := attrs[key]; got != want {
		t.Fatalf("attr %s = %q, want %q", key, got, want)
	}
}
