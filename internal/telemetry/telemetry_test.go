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

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
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
		WorkflowDigest:  "sha256:workflow",
		RunID:           runID,
		ItemID:          "42",
		ItemURL:         "https://github.com/acme/web/issues/42",
	})
	if err != nil {
		t.Fatalf("StartRun() error = %v", err)
	}

	_, taskSpan, err := client.StartTask(runCtx, TaskAttributes{
		Gaggle:      "acme-web",
		WorkflowID:  "default-implement",
		RunID:       runID,
		TaskID:      "implement",
		TaskType:    "agentic",
		GooberID:    "coder",
		Attempt:     2,
		AttemptKind: AttemptKindPolicy,
	})
	if err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}
	taskSpan.Event("tool.completed", attribute.String("tool.name", "go-test"))
	taskSpan.Succeed("task completed")

	_, gateSpan, err := client.StartGate(runCtx, GateAttributes{
		Gaggle:       "acme-web",
		WorkflowID:   "default-implement",
		RunID:        runID,
		GateID:       "qa",
		Decision:     "pass",
		RepassNumber: 1,
		GooberID:     "reviewer",
	})
	if err != nil {
		t.Fatalf("StartGate() error = %v", err)
	}
	gateSpan.SetGateResult("pass", 1)
	gateSpan.Complete(OutcomeSuccess, false)
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
	assertAttr(t, runAttrs, AttrGaggle, "acme-web")
	assertAttr(t, runAttrs, AttrWorkflow, "default-implement")
	assertAttr(t, runAttrs, AttrWorkflowVersion, "v7")
	assertAttr(t, runAttrs, AttrWorkflowDigest, "sha256:workflow")
	assertAttr(t, runAttrs, AttrRunID, runID)
	assertAttr(t, runAttrs, AttrItemID, "42")
	assertAttr(t, runAttrs, AttrItemURL, "https://github.com/acme/web/issues/42")

	taskAttrs := attrMap(task)
	assertAttr(t, taskAttrs, AttrStage, "implement")
	assertAttr(t, taskAttrs, AttrStageType, "agentic")
	assertAttr(t, taskAttrs, AttrGoober, "coder")
	assertAttr(t, taskAttrs, AttrAttemptNumber, "2")
	assertAttr(t, taskAttrs, AttrAttemptKind, AttemptKindPolicy)
	assertAttr(t, taskAttrs, AttrOutcome, OutcomeSuccess)
	if task.Status().Code != codes.Ok {
		t.Fatalf("task status = %s, want OK", task.Status().Code)
	}
	if len(task.Events()) != 1 || task.Events()[0].Name != "tool.completed" {
		t.Fatalf("task events = %#v, want tool.completed event", task.Events())
	}

	gateAttrs := attrMap(gate)
	assertAttr(t, gateAttrs, AttrStage, "qa")
	assertAttr(t, gateAttrs, AttrStageType, StageTypeGate)
	assertAttr(t, gateAttrs, AttrGoober, "reviewer")
	assertAttr(t, gateAttrs, AttrGateDecision, "pass")
	assertAttr(t, gateAttrs, AttrGateRepassNumber, "1")
	assertAttr(t, gateAttrs, AttrOutcome, OutcomeSuccess)
}

func TestSpanEventLimitBoundsAttemptAccumulation(t *testing.T) {
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
		Gaggle:     "acme-web",
		WorkflowID: "default-implement",
		Action:     "claim",
		ItemID:     "42",
	})
	if err != nil {
		t.Fatalf("StartSchedulerSpan() error = %v", err)
	}
	span.End()

	scheduler := findSpan(t, exporter.Spans(), "scheduler/claim")
	attrs := attrMap(scheduler)
	assertAttr(t, attrs, AttrStage, "claim")
	assertAttr(t, attrs, AttrStageType, StageTypeScheduler)
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
	if got := attrMap(task)[AttrErrorType]; got == "" {
		t.Fatal("task error.type is empty")
	}
}

func TestClientScrubsRegisteredCredentialBeforeExport(t *testing.T) {
	const secret = "opaque-encoded-ado-credential"
	registry, scrubber := journal.DefaultScrubber()
	registry.Register([]byte(secret))
	exporter := NewMemoryExporter()
	client, err := New(context.Background(), Config{
		ServiceName:  "telemetry-test",
		SpanExporter: exporter,
		Scrubber:     scrubber,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	_, span, err := client.StartSchedulerSpan(context.Background(), SchedulerAttributes{
		Gaggle: "acme-web", WorkflowID: "wf", Action: secret,
	})
	if err != nil {
		t.Fatalf("StartSchedulerSpan() error = %v", err)
	}
	span.Event("provider.request", attribute.String("authorization", secret))
	span.Fail(errors.New("provider failed with " + secret))

	exported := exporter.Spans()[0]
	if strings.Contains(exported.Status().Description, secret) {
		t.Fatalf("registered credential leaked into span status: %q", exported.Status().Description)
	}
	for key, value := range attrMap(exported) {
		if strings.Contains(value, secret) {
			t.Fatalf("registered credential leaked into span attribute %q: %q", key, value)
		}
	}
	for _, event := range exported.Events() {
		for _, attr := range event.Attributes {
			if strings.Contains(attr.Value.Emit(), secret) {
				t.Fatalf("registered credential leaked into span event: %+v", event)
			}
		}
	}
}

// TestSpanCompleteRecordsOutcome is issue #710's span-fix acceptance:
// a business failure (isFailure=true) sets OTel status codes.Error — the
// prior span.Succeed(status) call reported codes.Ok for a failed run/stage
// span, so `goobers trace`/rollup span queries couldn't distinguish a failed
// run from a healthy one without reading free-text. Complete's second
// property — the canonical goobers.outcome attribute — is what an ok/success
// business status also needs recorded (not just the failure path), so a
// rollup consumer can query the actual outcome vocabulary
// (success/failed/completed/escalated/aborted) independent of OTel's own
// coarser two-value axis.
func TestSpanCompleteRecordsOutcome(t *testing.T) {
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
	failedTask.CompleteWithError("failure", "stage.failed", true)

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
	if got := attrMap(failed)[AttrOutcome]; got != "failure" {
		t.Fatalf("failed task %s attribute = %q, want failure", AttrOutcome, got)
	}
	if got := attrMap(failed)[AttrErrorCode]; got != "stage.failed" {
		t.Fatalf("failed task %s attribute = %q, want stage.failed", AttrErrorCode, got)
	}
	if got := attrMap(failed)[AttrErrorType]; got != "stage.failed" {
		t.Fatalf("failed task %s attribute = %q, want stage.failed", AttrErrorType, got)
	}

	ok := findSpan(t, spans, "task/implement")
	if ok.Status().Code != codes.Ok {
		t.Fatalf("ok task status = %s, want Ok", ok.Status().Code)
	}
	if got := attrMap(ok)[AttrOutcome]; got != "success" {
		t.Fatalf("ok task %s attribute = %q, want success", AttrOutcome, got)
	}

	run := findSpan(t, spans, "run/wf")
	if run.Status().Code != codes.Error {
		t.Fatalf("run status = %s, want Error — the run's OWN terminal phase was failed", run.Status().Code)
	}
	if got := attrMap(run)[AttrOutcome]; got != "failed" {
		t.Fatalf("run %s attribute = %q, want failed", AttrOutcome, got)
	}
}

func TestCanonicalAttributeRegistryDoesNotDrift(t *testing.T) {
	want := []string{
		"goobers.run.id",
		"goobers.gaggle",
		"goobers.workflow",
		"goobers.workflow.version",
		"goobers.workflow.digest",
		"goobers.goober",
		"goobers.stage",
		"goobers.stage.type",
		"goobers.attempt.n",
		"goobers.attempt.kind",
		"goobers.item.id",
		"goobers.item.url",
		"goobers.outcome",
		"goobers.error.code",
		"goobers.gate.decision",
		"goobers.gate.repass.n",
		"error.type",
	}
	got := AllAttributes()
	if len(got) != len(want) {
		t.Fatalf("attribute registry size = %d, want %d: %v", len(got), len(want), got)
	}
	seen := make(map[Attribute]bool, len(got))
	for i, attr := range got {
		if seen[attr] {
			t.Fatalf("attribute registry contains duplicate %q", attr)
		}
		seen[attr] = true
		if string(attr) != want[i] {
			t.Errorf("attribute %d = %q, want %q", i, attr, want[i])
		}
		if !KnownAttribute(string(attr)) {
			t.Errorf("KnownAttribute(%q) = false", attr)
		}
	}

	for _, legacy := range []string{"gaggle", "workflowId", "runId", "goobers.span.kind", "goobers.business_status"} {
		if KnownAttribute(legacy) {
			t.Errorf("legacy attribute %q remains registered", legacy)
		}
	}
}

func TestCanonicalAttributeValuesMatchRuntimeContracts(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"deterministic stage", string(apiv1.TaskDeterministic), StageTypeDeterministic},
		{"agentic stage", string(apiv1.TaskAgentic), StageTypeAgentic},
		{"policy attempt", string(journal.AttemptPolicy), AttemptKindPolicy},
		{"infrastructure attempt", string(journal.AttemptInfra), AttemptKindInfra},
		{"success outcome", string(apiv1.ResultSuccess), OutcomeSuccess},
		{"failure outcome", string(apiv1.ResultFailure), OutcomeFailure},
		{"blocked outcome", string(apiv1.ResultBlocked), OutcomeBlocked},
		{"pass decision", string(apiv1.VerdictPass), "pass"},
		{"fail decision", string(apiv1.VerdictFail), "fail"},
		{"needs-changes decision", string(apiv1.VerdictNeedsChanges), "needs-changes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("runtime value = %q, canonical telemetry value = %q", tc.got, tc.want)
			}
		})
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
		attrs[string(attr.Key)] = attr.Value.Emit()
	}
	return attrs
}

func assertAttr(t *testing.T, attrs map[string]string, key, want string) {
	t.Helper()
	if got := attrs[key]; got != want {
		t.Fatalf("attr %s = %q, want %q", key, got, want)
	}
}
