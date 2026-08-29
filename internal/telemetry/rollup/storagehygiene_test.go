package rollup

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/telemetry"
)

// Tests for the telemetry storage hygiene fix (audit 2026-08-08): a
// byte-offset cursor for scheduler spans (mirroring the existing events
// cursor), and a size cap on the free-text error message columns ingest
// writes.

// --- spans cursor -----------------------------------------------------

// schedulerSpanRecord builds one deterministic scheduler-kind span sharing
// fixtureRunID as its trace id, so a single db.Spans(ctx, fixtureRunID) query
// can see every span these tests write regardless of which ingest cycle
// inserted it.
func schedulerSpanRecord(spanID string, offsetSeconds int) telemetry.SpanRecord {
	start := fixtureStart.Add(time.Duration(offsetSeconds) * time.Second)
	return telemetry.SpanRecord{
		Schema: telemetry.SpanSchema, TraceID: fixtureRunID, SpanID: spanID,
		Name: "scheduler/dispatch", Kind: telemetry.SpanKindScheduler, Status: "ok",
		StartTime: start, EndTime: start.Add(time.Millisecond),
	}
}

// writeSchedulerSpanRecords (re)writes scheduler/spans/spans.jsonl from
// scratch with exactly these records, one JSON line each. Callers that want
// to simulate the real append-only writer pass a growing slice whose earlier
// elements are unchanged from the previous call — json.Marshal is
// deterministic per value, so those lines re-encode byte-identically and the
// file's prefix bytes stay stable, exactly like the real
// JournalSpanExporter.appendSpans growing the file underneath a running
// cursor.
func writeSchedulerSpanRecords(t *testing.T, schedulerDir string, records []telemetry.SpanRecord) error {
	t.Helper()
	dir := filepath.Join(schedulerDir, dirSpans)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	lines := make([]string, 0, len(records))
	for _, r := range records {
		body, err := json.Marshal(r)
		if err != nil {
			return err
		}
		lines = append(lines, string(body))
	}
	content := strings.Join(lines, "\n") + "\n"
	return os.WriteFile(filepath.Join(dir, fileSpans), []byte(content), 0o644)
}

func spansCursorRow(t *testing.T, db *DB) (byteOffset int64, present bool) {
	t.Helper()
	err := db.sql.QueryRow(`SELECT byte_offset FROM spans_ingest_cursor WHERE id = 1`).Scan(&byteOffset)
	if err != nil {
		return 0, false
	}
	return byteOffset, true
}

func countSpans(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM spans`).Scan(&n); err != nil {
		t.Fatalf("count spans: %v", err)
	}
	return n
}

// TestIngestSchedulerLogSpansCursorAdvancesAcrossCycles is the spans
// counterpart of TestIngestSchedulerLogAppendsIncrementally: re-ingesting
// after the spans file grows adds only the new span, and the cursor advances
// by EXACTLY the byte length of the appended line — proving (via a
// file-offset check, not just row counts) that the second cycle read only
// the delta, not the whole (now three-span) file from byte 0.
func TestIngestSchedulerLogSpansCursorAdvancesAcrossCycles(t *testing.T) {
	tmp := t.TempDir()
	schedulerDir := filepath.Join(tmp, "scheduler")
	first := []telemetry.SpanRecord{
		schedulerSpanRecord("aaaaaaaaaaaaaaaa", 1),
		schedulerSpanRecord("bbbbbbbbbbbbbbbb", 2),
	}
	if err := writeSchedulerSpanRecords(t, schedulerDir, first); err != nil {
		t.Fatal(err)
	}

	db := openTestDB(t, tmp)
	if err := db.IngestSchedulerLog(context.Background(), schedulerDir); err != nil {
		t.Fatalf("first IngestSchedulerLog: %v", err)
	}
	if got := countSpans(t, db); got != 2 {
		t.Fatalf("spans after first ingest = %d, want 2", got)
	}
	offset1, present := spansCursorRow(t, db)
	if !present || offset1 <= 0 {
		t.Fatalf("spans cursor after first ingest = (offset %d, present %v), want offset>0", offset1, present)
	}

	third := schedulerSpanRecord("cccccccccccccccc", 3)
	thirdLine, err := json.Marshal(third)
	if err != nil {
		t.Fatal(err)
	}
	wantAdvance := int64(len(thirdLine)) + 1 // + the line's trailing newline

	all := append(append([]telemetry.SpanRecord{}, first...), third)
	if err := writeSchedulerSpanRecords(t, schedulerDir, all); err != nil {
		t.Fatal(err)
	}
	if err := db.IngestSchedulerLog(context.Background(), schedulerDir); err != nil {
		t.Fatalf("second IngestSchedulerLog: %v", err)
	}
	if got := countSpans(t, db); got != 3 {
		t.Fatalf("spans after incremental ingest = %d, want 3 (no dupes)", got)
	}
	offset2, _ := spansCursorRow(t, db)
	if offset2-offset1 != wantAdvance {
		t.Fatalf("spans cursor advanced by %d bytes, want exactly %d (the appended span line only)", offset2-offset1, wantAdvance)
	}
}

// TestIngestSchedulerLogSpansCursorNoOpReingestIsIdempotent proves a
// steady-state ingest with no new spans neither duplicates rows nor moves the
// cursor — the property that stops the per-cycle O(all-spans-ever) rescan.
func TestIngestSchedulerLogSpansCursorNoOpReingestIsIdempotent(t *testing.T) {
	tmp := t.TempDir()
	schedulerDir := filepath.Join(tmp, "scheduler")
	records := []telemetry.SpanRecord{
		schedulerSpanRecord("aaaaaaaaaaaaaaaa", 1),
		schedulerSpanRecord("bbbbbbbbbbbbbbbb", 2),
	}
	if err := writeSchedulerSpanRecords(t, schedulerDir, records); err != nil {
		t.Fatal(err)
	}
	db := openTestDB(t, tmp)
	if err := db.IngestSchedulerLog(context.Background(), schedulerDir); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	offset1, _ := spansCursorRow(t, db)

	for i := 0; i < 3; i++ {
		if err := db.IngestSchedulerLog(context.Background(), schedulerDir); err != nil {
			t.Fatalf("re-ingest %d: %v", i, err)
		}
	}
	if got := countSpans(t, db); got != 2 {
		t.Fatalf("spans after no-op re-ingests = %d, want 2 (no dupes)", got)
	}
	offset2, _ := spansCursorRow(t, db)
	if offset2 != offset1 {
		t.Fatalf("spans cursor drifted on no-op re-ingest: %d -> %d", offset1, offset2)
	}
}

// TestIngestSchedulerLogSpansCursorResumesAfterFileShrinks covers rotation/
// compaction of the spans file: when it is now shorter than the last resume
// offset, ingest re-reads from the head instead of erroring or losing data —
// already-stored spans are retained (nothing prunes the spans table here) and
// the newly (re-)read ones are added, exactly the events-cursor precedent.
func TestIngestSchedulerLogSpansCursorResumesAfterFileShrinks(t *testing.T) {
	tmp := t.TempDir()
	schedulerDir := filepath.Join(tmp, "scheduler")
	original := []telemetry.SpanRecord{
		schedulerSpanRecord("aaaaaaaaaaaaaaaa", 1),
		schedulerSpanRecord("bbbbbbbbbbbbbbbb", 2),
		schedulerSpanRecord("cccccccccccccccc", 3),
	}
	if err := writeSchedulerSpanRecords(t, schedulerDir, original); err != nil {
		t.Fatal(err)
	}
	db := openTestDB(t, tmp)
	if err := db.IngestSchedulerLog(context.Background(), schedulerDir); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if got := countSpans(t, db); got != 3 {
		t.Fatalf("spans after first ingest = %d, want 3", got)
	}

	// The on-disk file is replaced by something far shorter than the offset
	// we last resumed at, forcing readSchedulerSpansFrom's reset-to-head path.
	replacement := []telemetry.SpanRecord{schedulerSpanRecord("dddddddddddddddd", 4)}
	if err := writeSchedulerSpanRecords(t, schedulerDir, replacement); err != nil {
		t.Fatal(err)
	}
	if err := db.IngestSchedulerLog(context.Background(), schedulerDir); err != nil {
		t.Fatalf("post-shrink ingest: %v", err)
	}
	if got := countSpans(t, db); got != 4 {
		t.Fatalf("spans after shrink+ingest = %d, want 4 (3 retained + 1 new)", got)
	}
	offset, present := spansCursorRow(t, db)
	if !present {
		t.Fatal("spans cursor missing after shrink+ingest")
	}
	info, err := os.Stat(filepath.Join(schedulerDir, dirSpans, fileSpans))
	if err != nil {
		t.Fatal(err)
	}
	if offset != info.Size() {
		t.Fatalf("spans cursor after shrink = %d, want file size %d", offset, info.Size())
	}
}

// --- message cap --------------------------------------------------------

// TestIngestSchedulerLogCapsOversizedErrorMessage is the #16-18 audit
// acceptance test: a scheduler_errors row built from a pathological
// oversized message (the real Jul 21-22 incident wrote a 2.6MB
// errors.Join'd message) is capped rather than stored whole.
func TestIngestSchedulerLogCapsOversizedErrorMessage(t *testing.T) {
	tmp := t.TempDir()
	schedulerDir := filepath.Join(tmp, "scheduler")
	huge := strings.Repeat("x", maxStoredMessageLen*2)
	line := instanceEventLine(1, "error", `"error":{"code":"stalled_run_sweep_failed","message":`+strconv.Quote(huge)+`}`)
	if err := writeInstanceEvents(t, schedulerDir, []string{line}); err != nil {
		t.Fatal(err)
	}

	db := openTestDB(t, tmp)
	if err := db.IngestSchedulerLog(context.Background(), schedulerDir); err != nil {
		t.Fatalf("IngestSchedulerLog: %v", err)
	}

	var stored string
	if err := db.sql.QueryRow(`SELECT message FROM scheduler_errors WHERE seq = 1`).Scan(&stored); err != nil {
		t.Fatalf("query scheduler_errors: %v", err)
	}
	// "x" repeated never matches a redaction pattern, so Redact is a no-op
	// here and capMessage(huge) is exactly the expected stored value.
	if want := capMessage(huge); stored != want {
		t.Fatalf("stored scheduler_errors.message (len %d) != capMessage(huge) (len %d)", len(stored), len(want))
	}
	if len(stored) > maxStoredMessageLen {
		t.Fatalf("stored scheduler_errors.message length = %d, exceeds cap %d", len(stored), maxStoredMessageLen)
	}
}

// TestIngestRunCapsOversizedRunErrorMessage proves the same cap applies to
// run_errors.message, the sibling free-text column the standalone eventError
// path writes (ingest.go's insertEvents, not just IngestSchedulerLog's
// scheduler_errors path).
func TestIngestRunCapsOversizedRunErrorMessage(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	dir := filepath.Join(runsDir, fixtureRunID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	runYAML := fmt.Sprintf(`schema: goobers.dev/journal/run/v1
runId: %s
workflow: implement
workflowVersion: 3
gaggle: web
startedAt: %s
`, fixtureRunID, fixtureStart.UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, fileRunYAML), []byte(runYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	huge := strings.Repeat("y", maxStoredMessageLen*2)
	lines := []string{
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":1,"branch":0,"time":%q,"type":"run.started"}`,
			fixtureStart.UTC().Format(time.RFC3339Nano)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":2,"branch":0,"time":%q,"type":"error","error":{"code":"claim_recovery_failed","message":%s}}`,
			fixtureStart.Add(time.Second).UTC().Format(time.RFC3339Nano), strconv.Quote(huge)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":3,"branch":0,"time":%q,"type":"run.finished","status":"failed"}`,
			fixtureStart.Add(2*time.Second).UTC().Format(time.RFC3339Nano)),
	}
	if err := os.WriteFile(filepath.Join(dir, fileEvents), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	db := openTestDB(t, tmp)
	if err := db.IngestRun(context.Background(), dir); err != nil {
		t.Fatalf("IngestRun: %v", err)
	}
	var stored string
	if err := db.sql.QueryRow(`SELECT message FROM run_errors WHERE run_id = ? AND seq = 2`, fixtureRunID).Scan(&stored); err != nil {
		t.Fatalf("query run_errors: %v", err)
	}
	if want := capMessage(huge); stored != want {
		t.Fatalf("stored run_errors.message (len %d) != capMessage(huge) (len %d)", len(stored), len(want))
	}
}

// --- capMessage (pure function) -----------------------------------------

func TestCapMessageLeavesShortMessageUnchanged(t *testing.T) {
	short := "claims lock operation pr-claim.count timed out"
	if got := capMessage(short); got != short {
		t.Fatalf("capMessage(%q) = %q, want unchanged", short, got)
	}
}

func TestCapMessageTruncatesPreservingPrefixAndMarker(t *testing.T) {
	original := strings.Repeat("a", maxStoredMessageLen*3)
	got := capMessage(original)

	if len(got) > maxStoredMessageLen {
		t.Fatalf("capMessage output length = %d, exceeds cap %d", len(got), maxStoredMessageLen)
	}
	sum := sha256.Sum256([]byte(original))
	wantMarker := fmt.Sprintf("...[truncated %d bytes, sha256:%x]", len(original), sum[:8])
	if !strings.HasSuffix(got, wantMarker) {
		t.Fatalf("capMessage output = %q, want suffix %q", got, wantMarker)
	}
	prefix := strings.TrimSuffix(got, wantMarker)
	if !strings.HasPrefix(original, prefix) {
		t.Fatalf("capMessage output prefix %q is not a prefix of the original message", prefix)
	}
	if len(prefix) == 0 {
		t.Fatal("capMessage kept zero bytes of the original message — marker alone is not forensically useful")
	}
}

// TestCapMessageDoesNotSplitAMultiByteRune constructs a message whose naive
// byte-offset cut point (maxStoredMessageLen - len(marker)) lands in the
// middle of a multi-byte UTF-8 rune, and checks capMessage backs off to a
// rune boundary instead of emitting invalid UTF-8.
func TestCapMessageDoesNotSplitAMultiByteRune(t *testing.T) {
	// A 3-byte rune (é encoded as U+00E9 is 2 bytes; use a guaranteed 3-byte
	// rune, e.g. U+2603 SNOWMAN) repeated so every candidate cut offset near
	// the limit falls inside some rune's byte sequence unless the function
	// actively backs off.
	original := strings.Repeat("☃", maxStoredMessageLen)
	got := capMessage(original)
	if !utf8.ValidString(got) {
		t.Fatalf("capMessage output is not valid UTF-8: %q", got)
	}
}
