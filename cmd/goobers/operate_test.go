package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/telemetry"
)

func initDemo(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "demo")
	if code, _, stderr := runArgs(t, "init", root); code != 0 {
		t.Fatalf("init: code = %d, stderr = %q", code, stderr)
	}
	return root
}

// TestRunCompletesDeterministicWorkflow exercises `goobers run` end to end
// against the real runner (issue #23's daemon-loop follow-up rewired both
// `run` and `up` off the old escalation stub) — a deterministic-only fixture
// workflow so it needs neither a real Copilot CLI nor network access; see
// initDeterministicDemo in daemon_test.go.
func TestRunCompletesDeterministicWorkflow(t *testing.T) {
	root := initDeterministicDemo(t)

	code, stdout, stderr := runArgs(t, "run", "default-implement", root)
	if code != 0 {
		t.Fatalf("run: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "created run ") {
		t.Fatalf("run stdout = %q", stdout)
	}
	if !strings.Contains(stdout, "phase=completed") {
		t.Fatalf("expected the real runner to complete the deterministic task, stdout = %q", stdout)
	}

	// status lists the run, completed.
	code, stdout, stderr = runArgs(t, "status", root)
	if code != 0 {
		t.Fatalf("status: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "default-implement") || !strings.Contains(stdout, "completed") {
		t.Fatalf("status stdout = %q", stdout)
	}

	// trace shows the real stage dispatch sequence.
	runID := strings.Fields(strings.Split(stdout, "\n")[1])[0]
	code, stdout, stderr = runArgs(t, "trace", runID, root)
	if code != 0 {
		t.Fatalf("trace: code = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{"run.started", "stage.started", "local-ci", "stage.finished", "run.finished status=completed"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("trace stdout missing %q: %q", want, stdout)
		}
	}
}

// TestRunEmitsParseableSpans is issue #126's core acceptance: before this
// fix, no V0 binary ever constructed internal/telemetry.Client, so a real run
// never wrote runs/<id>/spans/spans.jsonl at all — `goobers trace` had
// nothing to enrich and the rollup's spans/span_events tables stayed
// permanently empty. `goobers run` against the deterministic fixture (single
// task, no gate) must now produce a parseable spans file with exactly one
// run-kind and one task-kind span, both on the run's own trace id.
func TestRunEmitsParseableSpans(t *testing.T) {
	root := initDeterministicDemo(t)

	code, stdout, stderr := runArgs(t, "run", "default-implement", root)
	if code != 0 {
		t.Fatalf("run: code = %d, stderr = %q", code, stderr)
	}
	firstLine := strings.Fields(strings.Split(stdout, "\n")[0])
	if len(firstLine) < 3 {
		t.Fatalf("unexpected run stdout = %q", stdout)
	}
	runID := firstLine[2]

	spansPath := filepath.Join(root, "runs", runID, "spans", "spans.jsonl")
	data, err := os.ReadFile(spansPath)
	if err != nil {
		t.Fatalf("read spans.jsonl: %v (a real run must emit spans now the telemetry client is wired)", err)
	}

	kinds := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var rec telemetry.SpanRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshal span line %q: %v", line, err)
		}
		if rec.Schema != telemetry.SpanSchema {
			t.Fatalf("span %q schema = %q, want %q", rec.Name, rec.Schema, telemetry.SpanSchema)
		}
		if rec.TraceID != runID {
			t.Fatalf("span %q traceId = %q, want run id %q", rec.Name, rec.TraceID, runID)
		}
		if rec.Status != "ok" {
			t.Fatalf("span %q status = %q, want ok", rec.Name, rec.Status)
		}
		kinds[rec.Kind]++
	}
	if kinds["run"] != 1 {
		t.Fatalf("span kinds = %v, want exactly one run span", kinds)
	}
	if kinds["task"] != 1 {
		t.Fatalf("span kinds = %v, want exactly one task span", kinds)
	}
}

// TestTraceShowsSpansWithoutPriorTelemetryCommand is issue #129's checklist:
// `goobers trace` reads telemetry.db directly (no Rebuild call of its own),
// so before #127/#128's incremental-ingest wiring, a fresh trace right after
// `goobers run` showed no spans section at all — it depended on a separate
// `goobers telemetry stats`/`errors` invocation having rebuilt the db first.
// This drives only run -> trace, deliberately never calling `telemetry`, to
// prove that dependency is gone.
func TestTraceShowsSpansWithoutPriorTelemetryCommand(t *testing.T) {
	root := initDeterministicDemo(t)

	code, stdout, stderr := runArgs(t, "run", "default-implement", root)
	if code != 0 {
		t.Fatalf("run: code = %d, stderr = %q", code, stderr)
	}
	runID := strings.Fields(strings.Split(stdout, "\n")[0])[2]

	code, stdout, stderr = runArgs(t, "trace", runID, root)
	if code != 0 {
		t.Fatalf("trace: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "\nspans:") {
		t.Fatalf("trace stdout missing a spans section without a prior `telemetry` call: %q", stdout)
	}
	if !strings.Contains(stdout, "run/default-implement") {
		t.Fatalf("trace stdout missing the run span: %q", stdout)
	}
}

// TestRunWithTelemetryDisabledSkipsSpansAndRollup is issue #129's
// telemetry.enabled defect: the config field was documented (and set in the
// real self-hosting config, selfhost/instance.yaml.example) but had zero
// callers — setting it to false did nothing. It's wired now, and the
// regression that would have shipped along with a naive wire-up is a
// typed-nil-in-interface panic (a nil *telemetry.Client assigned to
// runner.Config.Telemetry's SpanStarter interface field, or nil
// *rollup.DB passed through localscheduler.WithTelemetry, would make the
// runner's own `== nil` guard evaluate false and panic on first use) — this
// exercises the real `goobers run` path end-to-end with it off.
func TestRunWithTelemetryDisabledSkipsSpansAndRollup(t *testing.T) {
	root := initDeterministicDemo(t)
	instanceYAMLPath := filepath.Join(root, "instance.yaml")
	data, err := os.ReadFile(instanceYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	// `goobers init` already writes "telemetry: {}" (enabled defaults to
	// true) and a 0-byte telemetry.db placeholder (INST-010) — replace the
	// existing key rather than appending a duplicate one.
	body := strings.Replace(string(data), "telemetry: {}\n", "telemetry:\n  enabled: false\n", 1)
	if body == string(data) {
		t.Fatalf("expected instance.yaml to contain \"telemetry: {}\", got %q", data)
	}
	if err := os.WriteFile(instanceYAMLPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(root, "telemetry.db")
	before, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "run", "default-implement", root)
	if code != 0 {
		t.Fatalf("run: code = %d, stderr = %q", code, stderr)
	}
	firstLine := strings.Fields(strings.Split(stdout, "\n")[0])
	if len(firstLine) < 3 {
		t.Fatalf("unexpected run stdout = %q", stdout)
	}
	runID := firstLine[2]

	spansPath := filepath.Join(root, "runs", runID, "spans", "spans.jsonl")
	if _, err := os.Stat(spansPath); !os.IsNotExist(err) {
		t.Fatalf("spans.jsonl exists at %s with telemetry disabled, err = %v", spansPath, err)
	}
	// The placeholder telemetry.db `goobers init` writes is 0 bytes; opening
	// it for real (rollup.Open's WAL init + schema migration) would grow it.
	// Unchanged size is the signal rollup.Open was never called.
	after, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != 0 {
		t.Fatalf("test invariant broken: telemetry.db placeholder was not 0 bytes (%d)", before.Size())
	}
	if after.Size() != before.Size() {
		t.Fatalf("telemetry.db grew from %d to %d bytes with telemetry disabled — rollup.Open must not have been called", before.Size(), after.Size())
	}
}

func TestRunUnknownWorkflow(t *testing.T) {
	root := initDemo(t)
	code, _, stderr := runArgs(t, "run", "no-such-workflow", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr, `no workflow named "no-such-workflow"`) {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestRunMissingInstance(t *testing.T) {
	code, _, stderr := runArgs(t, "run", "anything", t.TempDir())
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "not an instance root") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestStatusEmptyInstance(t *testing.T) {
	root := initDemo(t)
	code, stdout, stderr := runArgs(t, "status", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "no runs found") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestTraceUnknownRun(t *testing.T) {
	root := initDemo(t)
	code, _, stderr := runArgs(t, "trace", "bogus-run-id", root)
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if stderr == "" {
		t.Fatalf("expected an error message on stderr")
	}
}

func TestTraceTooFewArgs(t *testing.T) {
	code, _, _ := runArgs(t, "trace")
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
}

// TestUpValidInstance-equivalent coverage (a valid instance's daemon starts,
// idles, and drains cleanly) now lives in daemon_test.go's
// TestUpIdlesThenDrainsOnCancel — `up` blocks on the real daemon loop, so it
// needs a cancellable context (runUpContext) rather than runArgs' synchronous
// signal-only runUp.

func TestUpMissingInstance(t *testing.T) {
	code, _, stderr := runArgs(t, "up", t.TempDir())
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "not an instance root") {
		t.Fatalf("stderr = %q", stderr)
	}
}
