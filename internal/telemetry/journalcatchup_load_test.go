package telemetry

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sqliteschema"
	"github.com/goobers/goobers/internal/sqliteuri"
)

func TestJournalCatchupFingerprintInvalidation(t *testing.T) {
	path, spool := t.TempDir(), t.TempDir()
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	since := time.Now().Add(-time.Hour)
	db, _, err := openJournalCursorStore(t.Context(), spool, since)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	client := journalTestClient(t, &journalTestExporter{})
	source := &journalCatchup{pipeline: client.journalLogs, root: path, spool: spool, since: since, maxAge: time.Hour}
	id := "0123456789abcdef0123456789abcdef"
	run, err := journal.Create(filepath.Join(path, "runs"), journal.RunIdentity{RunID: id, Workflow: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = run.Append(journal.Event{Type: journal.EventRunStarted}); err != nil {
		t.Fatal(err)
	}
	if err = run.Close(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join("runs", id)
	hint := journalCatchupHint{dir: dir}
	if _, err = source.processBatch(t.Context(), root, db, hint); err != nil {
		t.Fatal(err)
	}
	cursor, err := loadJournalCursor(t.Context(), db, dir)
	if err != nil || cursor.fingerprint == "" {
		t.Fatalf("cursor=%+v err=%v", cursor, err)
	}
	info, err := root.Stat(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if _, unchanged, err := unchangedJournalRun(t.Context(), root, dir, info, cursor); err != nil || !unchanged {
		t.Fatalf("unchanged EOF not cached: %v %v", unchanged, err)
	}
	accepted := client.journalLogs.accepted.Load()
	if _, err = source.processBatch(t.Context(), root, db, hint); err != nil {
		t.Fatal(err)
	}
	if client.journalLogs.accepted.Load() != accepted {
		t.Fatal("replayed unchanged history")
	}
	// An append after acknowledgement must invalidate EOF and export exactly once.
	line, err := json.Marshal(journal.Event{Schema: journal.EventSchema, Seq: cursor.seq + 1, Time: time.Now(), Type: journal.EventRunStarted})
	if err != nil {
		t.Fatal(err)
	}
	f, err := root.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Write(append(line, '\n'))
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err = source.processBatch(t.Context(), root, db, hint); err != nil {
			t.Fatal(err)
		}
	}
	if client.journalLogs.accepted.Load() != accepted+1 {
		t.Fatal("append was lost or duplicated")
	}
	// Schema edits invalidate the cache even when events are unchanged.
	if err = os.WriteFile(filepath.Join(path, dir, "schema.json"), []byte(`{"unsupported":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = source.processBatch(t.Context(), root, db, hint); err == nil {
		t.Fatal("cache hid unsupported schema")
	}
}

func TestJournalCatchupCursorV1Migration(t *testing.T) {
	path := t.TempDir()
	db, err := sql.Open("sqlite", sqliteuri.File(filepath.Join(path, ".journal-cursors.db"))+"?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err = sqliteschema.Migrate(t.Context(), db, "journal-export-cursors", []string{journalCursorSchema}); err != nil {
		t.Fatal(err)
	}
	since := time.Now().Add(-time.Hour)
	if _, err = db.ExecContext(t.Context(), "INSERT INTO enrollment VALUES(1,?)", since.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(t.Context(), "INSERT INTO cursors VALUES('runs/id','id','events.jsonl',123,7,?)", time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, enrolled, err := openJournalCursorStore(t.Context(), path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if !enrolled.Equal(since) {
		t.Fatal("migration changed enrollment")
	}
	cursor, err := loadJournalCursor(t.Context(), db, "runs/id")
	if err != nil || cursor != (journalCursor{identity: "id", generation: "events.jsonl", offset: 123, seq: 7}) {
		t.Fatalf("migration changed position or trusted an unvalidated fingerprint: %+v %v", cursor, err)
	}
	cursor.fingerprint = "validated"
	if err = saveJournalCursor(t.Context(), db, "runs/id", cursor); err != nil {
		t.Fatal(err)
	}
	if got, err := loadJournalCursor(t.Context(), db, "runs/id"); err != nil || got != cursor {
		t.Fatalf("fingerprint round trip: %+v %v", got, err)
	}
}

// Model already-acknowledged, enrolled history. Pre-enrollment or expired
// fixtures take an earlier mtime shortcut and do not exercise this path.
func BenchmarkJournalCatchupAcknowledgedHistory(b *testing.B) {
	benchmarkJournalCatchupHistory(b, 128, 1)
}

// Unlike the small warm-cache model above, this creates 14,400 distinct run
// directories and measures a complete acknowledged-history sweep per operation.
// Fixture creation, enrollment and the initial fingerprint pass are excluded.
// It measures the reader/cursor path, not the paced discovery goroutine or a
// daemon's concurrent workflow latency. Run explicitly with -benchtime=3x.
func BenchmarkJournalCatchupRetainedDirectorySweep(b *testing.B) {
	benchmarkJournalCatchupHistory(b, 14400, 14400)
}

func benchmarkJournalCatchupHistory(b *testing.B, fixtureCount, visitsPerOperation int) {
	b.Helper()
	path, spool := b.TempDir(), b.TempDir()
	root, err := os.OpenRoot(path)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	since := time.Now().Add(-time.Hour)
	db, _, err := openJournalCursorStore(b.Context(), spool, since)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	pipeline := newJournalLogPipeline(&journalTestExporter{}, resource.Empty(), nil)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = pipeline.shutdown(ctx)
	}()
	source := &journalCatchup{pipeline: pipeline, root: path, spool: spool, since: since, maxAge: 72 * time.Hour}
	hints := make([]journalCatchupHint, fixtureCount)
	for i := range hints {
		id := fmt.Sprintf("%032x", i+1)
		run, err := journal.Create(filepath.Join(path, "runs"), journal.RunIdentity{RunID: id, Workflow: "fixture", Gaggle: "synthetic"}, nil)
		if err != nil {
			b.Fatal(err)
		}
		if err = run.Append(journal.Event{Type: journal.EventRunStarted}); err != nil {
			b.Fatal(err)
		}
		if err = run.Close(); err != nil {
			b.Fatal(err)
		}
		hints[i] = journalCatchupHint{dir: filepath.Join("runs", id)}
		batch, err := journal.ReadExportBatch(b.Context(), root, hints[i].dir, false, journal.ExportPosition{}, 0)
		if err != nil {
			b.Fatal(err)
		}
		cursor := journalCursor{identity: batch.Position.Identity, generation: batch.Position.Generation, offset: batch.Position.Offset, seq: batch.Position.Seq}
		if err = saveJournalCursor(b.Context(), db, hints[i].dir, cursor); err != nil {
			b.Fatal(err)
		}
		// Let any implemented fingerprint cache initialize before timing.
		if _, err = source.processBatch(b.Context(), root, db, hints[i]); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	started := time.Now()
	for i := range b.N {
		for j := range visitsPerOperation {
			if more, err := source.processBatch(b.Context(), root, db, hints[(i*visitsPerOperation+j)%fixtureCount]); err != nil || more {
				b.Fatalf("unchanged history: more=%v err=%v", more, err)
			}
		}
	}
	elapsed := time.Since(started)
	b.StopTimer()
	metric := "estimated-14400-sweep-ms"
	if visitsPerOperation == fixtureCount {
		metric = "actual-14400-sweep-ms"
	}
	b.ReportMetric(float64(elapsed.Nanoseconds())/float64(max(b.N, 1)*visitsPerOperation)*14400/1e6, metric)
	if pipeline.accepted.Load() != 0 {
		b.Fatal("acknowledged history was replayed")
	}
}
