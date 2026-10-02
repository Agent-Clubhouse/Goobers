package rollup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/telemetry"
)

// The torn-write shape from the #5562 incident: a complete, newline-terminated
// line that is not valid JSON.
const corruptSchedulerLine = `{"schema":"goobers.dev/journal/event/v1","seq":99 "stray`

// TestIngestSchedulerLogSkipsCorruptLineMidFile is the #5562 regression: one
// corrupt line in the middle of the scheduler event log and of the scheduler
// spans log used to fail every ingest at that line forever. It is now skipped
// and counted, every record after it is ingested, the cursors move past it,
// and the skip is recorded in telemetry.db where an operator can see it.
func TestIngestSchedulerLogSkipsCorruptLineMidFile(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	schedulerDir := filepath.Join(tmp, "scheduler")
	events := firstFive()
	// An undecodable line, and a decodable record with no schema at all:
	// both are corrupt, and neither may pin the cursor.
	events = append(events[:2], append([]string{corruptSchedulerLine, `{}`}, events[2:]...)...)
	if err := writeInstanceEvents(t, schedulerDir, events); err != nil {
		t.Fatal(err)
	}
	traceless := schedulerSpanRecord("cccccccccccccccc", 3)
	traceless.TraceID = ""
	if err := writeSchedulerSpanLines(t, schedulerDir,
		schedulerSpanRecord("aaaaaaaaaaaaaaaa", 1), corruptSchedulerLine, traceless,
		schedulerSpanRecord("bbbbbbbbbbbbbbbb", 2)); err != nil {
		t.Fatal(err)
	}
	db := openTestDB(t, tmp)
	if err := db.IngestSchedulerLog(ctx, schedulerDir); err != nil {
		t.Fatalf("IngestSchedulerLog with a corrupt mid-file line: %v", err)
	}
	if got := schedulerEventTypes(t, db); len(got) != 5 {
		t.Fatalf("events ingested = %v, want all 5 valid events around the corrupt line", got)
	}
	if got := countSpans(t, db); got != 2 {
		t.Fatalf("spans ingested = %d, want both valid spans around the corrupt line", got)
	}
	assertCursorAtEOF(t, db, schedulerDir)

	health, err := db.SchedulerIngestHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if health.SkippedRecords != 4 || health.LastSkipAt == nil ||
		!strings.Contains(health.LastSkip, "corrupt record at byte offset") {
		t.Fatalf("health = %+v, want 4 skipped records with a byte-offset example", health)
	}
	if health.FailingSince != nil {
		t.Fatalf("health.FailingSince = %v, want nil after a successful ingest", health.FailingSince)
	}

	// The cursor is past the bad line, so a re-ingest neither re-skips nor
	// re-counts it.
	if err := db.IngestSchedulerLog(ctx, schedulerDir); err != nil {
		t.Fatal(err)
	}
	again, err := db.SchedulerIngestHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again.SkippedRecords != 4 {
		t.Fatalf("skipped after re-ingest = %d, want still 4", again.SkippedRecords)
	}

	// A cancelled ingest is the caller stopping, not ingestion failing.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := db.IngestSchedulerLog(cancelled, schedulerDir); err == nil {
		t.Fatal("IngestSchedulerLog with a cancelled context succeeded")
	}
	afterCancel, err := db.SchedulerIngestHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if afterCancel.FailingSince != nil {
		t.Fatalf("a cancelled ingest recorded a failure: %+v", afterCancel)
	}
}

// TestSchedulerIngestHealthRecordsAndClearsAFailure proves a failing ingest is
// recorded durably (not only in the scheduler log it cannot read) and that the
// next successful ingest clears the failure streak.
func TestSchedulerIngestHealthRecordsAndClearsAFailure(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	schedulerDir := filepath.Join(tmp, "scheduler")
	// A schema this build does not support stays fatal by design.
	unsupported := strings.Replace(instanceEventLine(1, "trigger.fired", `"workflow":"nominate"`),
		"goobers.dev/journal/event/v1", "goobers.dev/journal/event/v99", 1)
	if err := writeInstanceEvents(t, schedulerDir, []string{unsupported}); err != nil {
		t.Fatal(err)
	}
	db := openTestDB(t, tmp)
	if err := db.IngestSchedulerLog(ctx, schedulerDir); err == nil {
		t.Fatal("IngestSchedulerLog accepted an unsupported event schema")
	}
	failing, err := db.SchedulerIngestHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if failing.FailingSince == nil || failing.LastFailureAt == nil || !strings.Contains(failing.LastFailure, "v99") {
		t.Fatalf("health after failure = %+v, want a recorded failure naming the schema", failing)
	}
	since := *failing.FailingSince
	if err := db.IngestSchedulerLog(ctx, schedulerDir); err == nil {
		t.Fatal("second ingest unexpectedly succeeded")
	}
	still, err := db.SchedulerIngestHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if still.FailingSince == nil || !still.FailingSince.Equal(since) {
		t.Fatalf("FailingSince = %v, want the streak start %v kept", still.FailingSince, since)
	}

	if err := writeInstanceEvents(t, schedulerDir, firstFive()); err != nil {
		t.Fatal(err)
	}
	if err := db.IngestSchedulerLog(ctx, schedulerDir); err != nil {
		t.Fatalf("ingest after repair: %v", err)
	}
	recovered, err := db.SchedulerIngestHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.FailingSince != nil || recovered.LastFailureAt == nil {
		t.Fatalf("health after recovery = %+v, want FailingSince cleared and LastFailureAt kept", recovered)
	}
}

func writeSchedulerSpanLines(t *testing.T, schedulerDir string, lines ...any) error {
	t.Helper()
	dir := filepath.Join(schedulerDir, dirSpans)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var body strings.Builder
	for _, line := range lines {
		switch value := line.(type) {
		case string:
			body.WriteString(value)
		case telemetry.SpanRecord:
			encoded, err := json.Marshal(value)
			if err != nil {
				return err
			}
			body.Write(encoded)
		}
		body.WriteByte('\n')
	}
	return os.WriteFile(filepath.Join(dir, fileSpans), []byte(body.String()), 0o644)
}

func assertCursorAtEOF(t *testing.T, db *DB, schedulerDir string) {
	t.Helper()
	eventsInfo, err := os.Stat(filepath.Join(schedulerDir, fileEvents))
	if err != nil {
		t.Fatal(err)
	}
	if offset, _, _ := schedulerCursorRow(t, db); offset != eventsInfo.Size() {
		t.Fatalf("event cursor = %d, want %d (past the corrupt line)", offset, eventsInfo.Size())
	}
	spansInfo, err := os.Stat(filepath.Join(schedulerDir, dirSpans, fileSpans))
	if err != nil {
		t.Fatal(err)
	}
	if offset, _ := spansCursorRow(t, db); offset != spansInfo.Size() {
		t.Fatalf("spans cursor = %d, want %d (past the corrupt line)", offset, spansInfo.Size())
	}
}
