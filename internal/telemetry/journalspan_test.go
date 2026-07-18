package telemetry

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"

	"github.com/goobers/goobers/internal/journal"
)

// newTestClient builds a Client wired to a JournalSpanExporter under dir, with
// synchronous export (no batching) so every span.End() is flushed to disk
// immediately, keeping tests deterministic without a Flush race.
func newTestClient(t *testing.T, dir string) (*Client, string) {
	t.Helper()
	runID, err := NewRunID()
	if err != nil {
		t.Fatalf("NewRunID: %v", err)
	}
	client, err := New(context.Background(), Config{
		ServiceName:  "goobers-test",
		SpanExporter: NewJournalSpanExporter(dir, nil),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })
	return client, runID
}

func readSpanRecords(t *testing.T, dir, runID string) []SpanRecord {
	t.Helper()
	path := filepath.Join(dir, runID, spansDirName, spanFileName)
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	var out []SpanRecord
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var rec SpanRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			t.Fatalf("decode span line: %v", err)
		}
		out = append(out, rec)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return out
}

func TestJournalSpanExporterWritesUnderRunSpansDir(t *testing.T) {
	dir := t.TempDir()
	client, runID := newTestClient(t, dir)

	ctx, runSpan, err := client.StartRun(context.Background(), RunAttributes{
		Gaggle: "web", WorkflowID: "implement", WorkflowVersion: "1", RunID: runID,
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	_, taskSpan, err := client.StartTask(ctx, TaskAttributes{
		Gaggle: "web", WorkflowID: "implement", WorkflowVersion: "1", RunID: runID,
		TaskID: "build", TaskType: "deterministic",
	})
	if err != nil {
		t.Fatalf("StartTask: %v", err)
	}
	taskSpan.Event("harness.tool_call", attribute.String("tool", "go test"))
	taskSpan.Succeed("build passed")
	runSpan.Succeed("run complete")

	recs := readSpanRecords(t, dir, runID)
	if len(recs) != 2 {
		t.Fatalf("len(records) = %d, want 2 (run + task)", len(recs))
	}

	var run, task *SpanRecord
	for i := range recs {
		switch recs[i].Kind {
		case SpanKindRun:
			run = &recs[i]
		case SpanKindTask:
			task = &recs[i]
		}
	}
	if run == nil || task == nil {
		t.Fatalf("expected a run span and a task span, got kinds: %v", []string{recs[0].Kind, recs[1].Kind})
	}
	if run.TraceID != runID || task.TraceID != runID {
		t.Fatalf("expected both spans under trace/run id %s: run=%s task=%s", runID, run.TraceID, task.TraceID)
	}
	if task.ParentSpanID != run.SpanID {
		t.Fatalf("expected task span's parent to be the run span: task.parent=%s run.span=%s", task.ParentSpanID, run.SpanID)
	}
	if run.Status != "ok" || task.Status != "ok" {
		t.Fatalf("expected ok status: run=%s task=%s", run.Status, task.Status)
	}
	if len(task.Events) != 1 || task.Events[0].Name != "harness.tool_call" {
		t.Fatalf("expected the within-stage harness event to attach to the task span: %#v", task.Events)
	}
	if task.Events[0].Attributes["tool"] != "go test" {
		t.Fatalf("expected event attribute preserved: %#v", task.Events[0].Attributes)
	}
}

func TestJournalSpanExporterAppendsAcrossExportCalls(t *testing.T) {
	dir := t.TempDir()
	client, runID := newTestClient(t, dir)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		rctx, run, err := client.StartRun(ctx, RunAttributes{Gaggle: "web", WorkflowID: "wf", WorkflowVersion: "1", RunID: runID})
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
		_ = rctx
		run.Succeed("ok")
	}
	recs := readSpanRecords(t, dir, runID)
	if len(recs) != 3 {
		t.Fatalf("expected 3 appended span records, got %d", len(recs))
	}
}

// TestJournalSpanExporterRedactsRegisteredSecret is the #117 Piece B negative
// control: an exporter given a registry-backed scrubber redacts a resolver-issued
// secret registered for a run — even a shapeless one the pattern net alone cannot
// catch. Before Piece B the exporter scrubbed pattern-only, so such a secret would
// have landed in spans.jsonl at rest.
func TestJournalSpanExporterRedactsRegisteredSecret(t *testing.T) {
	// Deliberately shapeless: no provider prefix, no key=value framing. The
	// pattern net can't catch it — only the registry can (mechanism isolation).
	const secret = "Kf9wQ2mNpZ7-internal-issued-value"
	if got := string(journal.NewPatternScrubber().Scrub([]byte(secret))); got != secret {
		t.Fatalf("precondition: the pattern net alone must NOT catch %q (got %q), else this does not isolate the registry", secret, got)
	}

	reg := journal.NewRegistryScrubber()
	reg.Register([]byte(secret))

	dir := t.TempDir()
	runID, err := NewRunID()
	if err != nil {
		t.Fatalf("NewRunID: %v", err)
	}
	client, err := New(context.Background(), Config{
		ServiceName:  "goobers-test",
		SpanExporter: NewJournalSpanExporter(dir, journal.Chain(reg, journal.NewPatternScrubber())),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	ctx, run, err := client.StartRun(context.Background(), RunAttributes{
		Gaggle: "web", WorkflowID: "wf", WorkflowVersion: "1", RunID: runID,
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	_, task, err := client.StartTask(ctx, TaskAttributes{
		Gaggle: "web", WorkflowID: "wf", WorkflowVersion: "1", RunID: runID, TaskID: "t",
	})
	if err != nil {
		t.Fatalf("StartTask: %v", err)
	}
	task.Event("provider."+secret, attribute.String("token."+secret, secret))
	task.Fail(fmt.Errorf("stage logged %s", secret))
	run.Fail(fmt.Errorf("run carrying %s", secret))

	raw, err := os.ReadFile(filepath.Join(dir, runID, spansDirName, spanFileName))
	if err != nil {
		t.Fatalf("read spans file: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("registered secret leaked into spans.jsonl — exporter bypassed the registry:\n%s", raw)
	}
	if !strings.Contains(string(raw), RedactedPlaceholder) {
		t.Fatalf("expected the redaction placeholder in spans.jsonl:\n%s", raw)
	}
}

func TestJournalSpanExporterRedactsSecrets(t *testing.T) {
	dir := t.TempDir()
	client, runID := newTestClient(t, dir)

	// A realistic GitHub token shape: ghp_ + 36 chars (journal's canonical net,
	// now shared by this package, matches the real 36+ length — #117).
	const canary = "ghp_0123456789abcdefghijklmnopqrstuvwxyz"
	ctx, run, err := client.StartRun(context.Background(), RunAttributes{
		Gaggle: "web", WorkflowID: "wf", WorkflowVersion: "1", RunID: runID,
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	_, task, err := client.StartTask(ctx, TaskAttributes{
		Gaggle: "web", WorkflowID: "wf", WorkflowVersion: "1", RunID: runID, TaskID: "t",
	})
	if err != nil {
		t.Fatalf("StartTask: %v", err)
	}
	task.Event("provider.request", attribute.String("authorization", "Bearer "+canary))
	task.Fail(fmt.Errorf("token leaked: %s", canary))
	run.Fail(fmt.Errorf("child task failed carrying %s", canary))

	// Read the raw file bytes, not just decoded records: the canary must never
	// touch disk, in any field.
	raw, err := os.ReadFile(filepath.Join(dir, runID, spansDirName, spanFileName))
	if err != nil {
		t.Fatalf("read spans file: %v", err)
	}
	if strings.Contains(string(raw), canary) {
		t.Fatalf("canary secret found at rest in spans.jsonl:\n%s", raw)
	}
	recs := readSpanRecords(t, dir, runID)
	for _, r := range recs {
		if strings.Contains(r.StatusMessage, canary) {
			t.Fatalf("canary leaked into status message: %q", r.StatusMessage)
		}
		for _, ev := range r.Events {
			for k, v := range ev.Attributes {
				if strings.Contains(v, canary) {
					t.Fatalf("canary leaked into event attribute %q: %q", k, v)
				}
			}
		}
	}
}
