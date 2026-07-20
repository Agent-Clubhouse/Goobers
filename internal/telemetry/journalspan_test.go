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

func TestPerGaggleJournalSpanExporterRoutesToScopedRunDirectory(t *testing.T) {
	root := t.TempDir()
	runID, err := NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(context.Background(), Config{
		ServiceName:  "goobers-test",
		SpanExporter: NewPerGaggleJournalSpanExporter(root, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	_, span, err := client.StartRun(context.Background(), RunAttributes{
		Gaggle: "alpha", WorkflowID: "implementation", RunID: runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	span.Succeed("done")

	path := filepath.Join(root, "gaggles", "alpha", "runs", runID, spansDirName, spanFileName)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("scoped spans file %s: %v", path, err)
	}
}

func TestPerGaggleJournalSpanExporterRoutesSchedulerSpanToInstanceJournal(t *testing.T) {
	root := t.TempDir()
	runID, err := NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(context.Background(), Config{
		ServiceName:  "goobers-test",
		SpanExporter: NewPerGaggleJournalSpanExporter(root, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	_, span, err := client.StartSchedulerSpan(context.Background(), SchedulerAttributes{
		Gaggle: "alpha", WorkflowID: "implementation", RunID: runID, Action: "dispatch",
	})
	if err != nil {
		t.Fatal(err)
	}
	span.Complete(OutcomeBlocked, false)

	path := filepath.Join(root, "scheduler", spansDirName, spanFileName)
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open scheduler spans: %v", err)
	}
	defer func() { _ = f.Close() }()
	var got SpanRecord
	if err := json.NewDecoder(f).Decode(&got); err != nil {
		t.Fatalf("decode scheduler span: %v", err)
	}
	if got.TraceID != runID || got.Kind != SpanKindScheduler || got.Name != "scheduler/dispatch" {
		t.Fatalf("scheduler span = %#v", got)
	}
	runDir := filepath.Join(root, "gaggles", "alpha", "runs", runID)
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Fatalf("scheduler span created run directory %s: %v", runDir, err)
	}
}

func TestPerGaggleJournalSpanExporterRoutesToRetainedFlatJournal(t *testing.T) {
	root := t.TempDir()
	runID, err := NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	run, err := journal.Create(filepath.Join(root, "runs"), journal.RunIdentity{
		RunID: runID, Workflow: "implementation", WorkflowVersion: 1, Gaggle: "alpha",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	client, err := New(context.Background(), Config{
		ServiceName:  "goobers-test",
		SpanExporter: NewPerGaggleJournalSpanExporter(root, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	_, span, err := client.StartRun(context.Background(), RunAttributes{
		Gaggle: "alpha", WorkflowID: "implementation", RunID: runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	span.Succeed("done")

	flatPath := filepath.Join(root, "runs", runID, spansDirName, spanFileName)
	if _, err := os.Stat(flatPath); err != nil {
		t.Fatalf("retained flat spans file %s: %v", flatPath, err)
	}
	scopedRun := filepath.Join(root, "gaggles", "alpha", "runs", runID)
	if _, err := os.Stat(scopedRun); !os.IsNotExist(err) {
		t.Fatalf("exporter created duplicate scoped run %s: %v", scopedRun, err)
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
	const basicCredential = "YnVpbGQtYWdlbnQ6YWRvLXBhdC0wMTIzNDU2Nzg5"

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
	task.Event("provider.basic_request", attribute.String("authorization", "Basic "+basicCredential))
	task.Fail(fmt.Errorf("tokens leaked: %s; Basic %s", canary, basicCredential))
	run.Fail(fmt.Errorf("child task failed carrying %s; Basic %s", canary, basicCredential))

	// Read the raw file bytes, not just decoded records: the canary must never
	// touch disk, in any field.
	raw, err := os.ReadFile(filepath.Join(dir, runID, spansDirName, spanFileName))
	if err != nil {
		t.Fatalf("read spans file: %v", err)
	}
	for _, secret := range []string{canary, basicCredential} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("credential found at rest in spans.jsonl:\n%s", raw)
		}
	}
	recs := readSpanRecords(t, dir, runID)
	basicAuthRedacted := false
	for _, r := range recs {
		for _, secret := range []string{canary, basicCredential} {
			if strings.Contains(r.StatusMessage, secret) {
				t.Fatalf("credential leaked into status message: %q", r.StatusMessage)
			}
		}
		for _, ev := range r.Events {
			for k, v := range ev.Attributes {
				for _, secret := range []string{canary, basicCredential} {
					if strings.Contains(v, secret) {
						t.Fatalf("credential leaked into event attribute %q: %q", k, v)
					}
				}
				if ev.Name == "provider.basic_request" && k == "authorization" && v == RedactedPlaceholder {
					basicAuthRedacted = true
				}
			}
		}
	}
	if !basicAuthRedacted {
		t.Fatal("Basic authorization span event was not redacted")
	}
}
