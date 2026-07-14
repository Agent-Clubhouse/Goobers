package rollup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/telemetry"
)

// writeRunWithRawEvents hand-writes run.yaml plus a caller-supplied raw
// events.jsonl body (and, if spansBody is non-empty, spans/spans.jsonl too) —
// for tests that need to control the exact on-disk bytes, torn tails
// included, rather than going through journal.Run's own writer.
func writeRunWithRawEvents(t *testing.T, runsDir, runID string, eventsBody, spansBody string) string {
	t.Helper()
	dir := filepath.Join(runsDir, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, fileRunYAML), []byte(minimalRunYAML(runID, fixtureStart)), 0o644); err != nil {
		t.Fatalf("write run.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, fileEvents), []byte(eventsBody), 0o644); err != nil {
		t.Fatalf("write events.jsonl: %v", err)
	}
	if spansBody != "" {
		spansDir := filepath.Join(dir, dirSpans)
		if err := os.MkdirAll(spansDir, 0o755); err != nil {
			t.Fatalf("mkdir spans dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(spansDir, fileSpans), []byte(spansBody), 0o644); err != nil {
			t.Fatalf("write spans.jsonl: %v", err)
		}
	}
	return dir
}

func rawEventLine(seq int, typ string) string {
	return fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":%d,"branch":0,"time":%q,"type":%q}`,
		seq, fixtureStart.Add(time.Duration(seq)*time.Second).UTC().Format(time.RFC3339Nano), typ)
}

func rawSpanLine(name string) string {
	return fmt.Sprintf(`{"schema":%q,"traceId":"4bf92f3577b34da6a3ce929d0e0e4736","spanId":"00f067aa0ba902b7","name":%q,"kind":"task","startTime":%q,"endTime":%q,"status":"ok"}`,
		telemetry.SpanSchema, name, fixtureStart.UTC().Format(time.RFC3339Nano), fixtureStart.Add(time.Second).UTC().Format(time.RFC3339Nano))
}

// TestFormatTimeIsFixedWidthForLexicographicOrder is issue #129's checklist:
// time.RFC3339Nano trims trailing fractional zeros, so two same-second
// timestamps can format to different-length strings — lexicographic ORDER BY
// / range comparisons (aggregates.go's Since/Until filters, query.go's
// ORDER BY occurred_at) then disagree with chronological order. formatTime's
// fixed-width layout must always emit the same length.
func TestFormatTimeIsFixedWidthForLexicographicOrder(t *testing.T) {
	base := fixtureStart
	times := []time.Time{
		base,                             // whole second: RFC3339Nano would trim ALL fractional digits
		base.Add(500 * time.Millisecond), // RFC3339Nano would trim to 1 digit
		base.Add(1 * time.Second),
	}
	var formatted []string
	for _, tm := range times {
		formatted = append(formatted, formatTime(tm).String)
	}
	for i, s := range formatted {
		if len(s) != len(formatted[0]) {
			t.Fatalf("formatTime(%v) = %q (len %d), want same length as %q (len %d) — mixed-width timestamps break lexicographic ordering",
				times[i], s, len(s), formatted[0], len(formatted[0]))
		}
	}
	if formatted[0] >= formatted[1] || formatted[1] >= formatted[2] {
		t.Fatalf("formatted timestamps not in lexicographic order: %v", formatted)
	}

	// Round-trip: parseTime must still read the fixed-width format back to
	// the exact same instant.
	for i, tm := range times {
		got, err := parseTime(formatTime(tm))
		if err != nil {
			t.Fatalf("parseTime: %v", err)
		}
		if !got.Equal(tm) {
			t.Fatalf("round-trip[%d]: got %v, want %v", i, got, tm)
		}
	}
}

// TestIngestRunToleratesTornEventsTail is issue #127's first defect: a run
// interrupted mid-append (crashed, not yet journal.Recover'd) leaves an
// incomplete final events.jsonl line with no trailing newline. Before this
// fix, rollup/reader.go's readEvents failed IngestRun on any undecodable
// line — one such crashed, unrecovered run made `goobers telemetry
// stats|errors` exit 2 for the WHOLE instance, not just that run. The torn
// tail must be silently dropped, exactly like internal/journal's own reader
// already does on the write side.
func TestIngestRunToleratesTornEventsTail(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	complete := strings.Join([]string{rawEventLine(1, "run.started"), rawEventLine(2, "run.finished")}, "\n") + "\n"
	torn := `{"schema":"goobers.dev/journal/event/v1","seq":3,"branch":0,"time":"2026-07-13T00:00:03Z","type":"stage.st` // no trailing newline, cut mid-field
	runDir := writeRunWithRawEvents(t, runsDir, fixtureRunID, complete+torn, "")

	db := openTestDB(t, tmp)
	if err := db.IngestRun(runDir); err != nil {
		t.Fatalf("IngestRun: want the torn tail tolerated, got error: %v", err)
	}

	runs, err := db.Runs()
	if err != nil || len(runs) != 1 {
		t.Fatalf("Runs: %v, %#v", err, runs)
	}
	if runs[0].Status != "" {
		t.Fatalf("run status = %q, want empty (run.finished IS present here — this just proves the torn 3rd line didn't corrupt the 2 complete ones)", runs[0].Status)
	}
}

// TestIngestRunToleratesTornSpansTail mirrors the events-tail case for
// spans.jsonl: JournalSpanExporter fsyncs after each ExportSpans batch, so an
// interrupted process leaves the same incomplete-final-line signature.
func TestIngestRunToleratesTornSpansTail(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	events := rawEventLine(1, "run.started") + "\n" + rawEventLine(2, "run.finished") + "\n"
	completeSpan := rawSpanLine("run/implement") + "\n"
	tornSpan := `{"schema":"goobers.dev/telemetry/span/v1","traceId":"4bf92f3577b34da6a3ce929d0e0e4736","spanId":"deadbeef","name":"task/bui`
	runDir := writeRunWithRawEvents(t, runsDir, fixtureRunID, events, completeSpan+tornSpan)

	db := openTestDB(t, tmp)
	if err := db.IngestRun(runDir); err != nil {
		t.Fatalf("IngestRun: want the torn spans tail tolerated, got error: %v", err)
	}

	spans, err := db.Spans(fixtureRunID)
	if err != nil {
		t.Fatalf("Spans: %v", err)
	}
	if len(spans) != 1 || spans[0].Name != "run/implement" {
		t.Fatalf("spans = %#v, want exactly the one complete span", spans)
	}
}

// TestIngestRunRejectsCorruptCompleteEventLine is the regression guard for
// TestIngestRunToleratesTornEventsTail: tolerance is scoped to the torn FINAL
// line only. A complete (newline-terminated) line that's genuinely corrupt —
// not a torn write — must still fail loudly, not be silently swallowed as if
// it were just another torn tail.
func TestIngestRunRejectsCorruptCompleteEventLine(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	body := rawEventLine(1, "run.started") + "\n" + `{not valid json` + "\n" + rawEventLine(3, "run.finished") + "\n"
	runDir := writeRunWithRawEvents(t, runsDir, fixtureRunID, body, "")

	db := openTestDB(t, tmp)
	if err := db.IngestRun(runDir); err == nil {
		t.Fatal("IngestRun: want an error for a corrupt COMPLETE line, got nil")
	}
}

// TestConcurrentIngestAndQueryUnderWAL is issue #127's concurrent-access
// acceptance criterion. Before this fix, openRollup ran rollup.Rebuild (an
// os.Remove of the shared telemetry.db) on every query, so two concurrent
// callers could unlink each other's database file mid-ingest, and no
// PRAGMA journal_mode=WAL/busy_timeout existed to let a reader and a writer
// coexist. Here N goroutines each open their OWN *DB handle against the same
// path (simulating separate `goobers up`/`goobers telemetry` processes) and
// concurrently ingest distinct runs and query Stats — none of that should
// ever error.
func TestConcurrentIngestAndQueryUnderWAL(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	dbPath := filepath.Join(tmp, "telemetry.db")

	const n = 8
	runDirs := make([]string, n)
	for i := 0; i < n; i++ {
		runID := fmt.Sprintf("%032x", i+1)
		events := rawEventLine(1, "run.started") + "\n" + rawEventLine(2, "run.finished") + "\n"
		runDirs[i] = writeRunWithRawEvents(t, runsDir, runID, events, "")
	}

	var wg sync.WaitGroup
	errs := make(chan error, n*2)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(dir string) {
			defer wg.Done()
			db, err := Open(dbPath)
			if err != nil {
				errs <- fmt.Errorf("open: %w", err)
				return
			}
			defer func() { _ = db.Close() }()
			if err := db.IngestRun(dir); err != nil {
				errs <- fmt.Errorf("ingest %s: %w", dir, err)
			}
		}(runDirs[i])

		wg.Add(1)
		go func() {
			defer wg.Done()
			db, err := Open(dbPath)
			if err != nil {
				errs <- fmt.Errorf("open for query: %w", err)
				return
			}
			defer func() { _ = db.Close() }()
			if _, err := db.Stats(StatsRequest{}); err != nil {
				errs <- fmt.Errorf("stats: %w", err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent access error: %v", err)
	}

	db := openTestDB(t, tmp)
	runs, err := db.Runs()
	if err != nil {
		t.Fatalf("Runs: %v", err)
	}
	if len(runs) != n {
		t.Fatalf("runs = %d, want %d (every concurrent ingest must have landed)", len(runs), n)
	}
}
