package telemetry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestJournalCatchupCursorOnlyAdvancesAfterAdmission(t *testing.T) {
	rootPath, spool := t.TempDir(), t.TempDir()
	root, err := os.OpenRoot(rootPath)
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
	exporter := &journalTestExporter{exportErr: errors.New("unavailable")}
	client := journalTestClient(t, exporter)
	source := &journalCatchup{pipeline: client.journalLogs, root: rootPath, spool: spool, since: since, maxAge: time.Hour}
	log, _, err := journal.OpenInstanceLog(filepath.Join(rootPath, "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	for range 17 {
		if err = log.Append(journal.Event{Type: journal.EventTickSkipped}); err != nil {
			t.Fatal(err)
		}
	}
	if err = log.Close(); err != nil {
		t.Fatal(err)
	}
	hint := journalCatchupHint{dir: "scheduler"}
	if _, err = source.processBatch(t.Context(), root, db, hint); err == nil {
		t.Fatal("failed batch advanced")
	}
	cursor, err := loadJournalCursor(t.Context(), db, "scheduler")
	if err != nil || cursor.seq != 0 {
		t.Fatalf("cursor=%+v err=%v", cursor, err)
	}
	exporter.mu.Lock()
	exporter.exportErr = nil
	exporter.records = nil
	exporter.mu.Unlock()
	if _, err = source.processBatch(t.Context(), root, db, hint); err != nil {
		t.Fatal(err)
	}
	cursor, err = loadJournalCursor(t.Context(), db, "scheduler")
	if err != nil || cursor.seq != 17 {
		t.Fatalf("cursor=%+v err=%v", cursor, err)
	}
	if _, err = source.processBatch(t.Context(), root, db, hint); err != nil {
		t.Fatal(err)
	}
	exporter.mu.Lock()
	defer exporter.mu.Unlock()
	if len(exporter.records) != 17 {
		t.Fatalf("cursor replayed acknowledged records: %d", len(exporter.records))
	}
	firstID := azureMonitorLog(&exporter.records[0]).Properties["goobers.telemetry.record_id"]
	if len(firstID) != 64 || firstID != azureMonitorLog(&exporter.records[0]).Properties["goobers.telemetry.record_id"] {
		t.Fatal("unstable journal record ID")
	}
}

func TestJournalCatchupProductionRegistrationAndRestart(t *testing.T) {
	root, spool := t.TempDir(), t.TempDir()
	// Create enrollment before the synthetic crash-window records, with no
	// running exporter: their only route to export is startup discovery.
	db, _, err := openJournalCursorStore(t.Context(), spool, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	log, _, err := journal.OpenInstanceLog(filepath.Join(root, "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if err = log.Append(journal.Event{Type: journal.EventTickSkipped}); err != nil {
			t.Fatal(err)
		}
	}
	_ = log.Close()
	receiver := &journalLoadReceiver{}
	base := newJournalLoadClient(t, receiver, func(cfg *Config) {
		cfg.JournalRoot, cfg.AzureMonitorReplayRoot, cfg.JournalInstanceID = root, spool, "instance"
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for base.JournalExportStats().Accepted < 10 {
		if !waitJournalCatchup(ctx, 10*time.Millisecond) {
			t.Fatal("startup did not recover pre-spool records")
		}
	}
	log, _, err = journal.OpenInstanceLog(filepath.Join(root, "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	if err = log.Append(journal.Event{Type: journal.EventTickSkipped}); err != nil {
		t.Fatal(err)
	}
	_ = log.Close()
	if err = base.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := settleJournalLoad(t, base).Accepted; got != 11 {
		t.Fatalf("live catch-up accepted=%d want=11", got)
	}
}

func TestJournalCatchupEnrollmentAndCursorRetention(t *testing.T) {
	path := t.TempDir()
	since := time.Now().Add(-time.Minute)
	db, first, err := openJournalCursorStore(t.Context(), path, since)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Equal(since) {
		t.Fatal("wrong enrollment boundary")
	}
	if err = saveJournalCursor(t.Context(), db, "old", journalCursor{identity: "id", seq: 3}); err != nil {
		t.Fatal(err)
	}
	if err = pruneJournalCursors(t.Context(), db, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if cursor, err := loadJournalCursor(t.Context(), db, "old"); err != nil || cursor.seq != 0 {
		t.Fatalf("cursor was not pruned: %+v %v", cursor, err)
	}
	_ = db.Close()
	db, second, err := openJournalCursorStore(t.Context(), path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if !second.Equal(first) {
		t.Fatal("restart changed enrollment boundary")
	}
}

func TestJournalCatchupCursorCountBoundAndNewerSchema(t *testing.T) {
	path := t.TempDir()
	db, _, err := openJournalCursorStore(t.Context(), path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	_, err = db.ExecContext(t.Context(), `WITH RECURSIVE n(x) AS
 (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<100010)
 INSERT INTO cursors(path,identity,generation,offset,seq,touched) SELECT CAST(x AS TEXT),'identity','events',0,x,?+x FROM n`, time.Now().UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	if err = pruneJournalCursors(t.Context(), db, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM cursors").Scan(&count); err != nil || count != 100000 {
		t.Fatalf("cursor bound: %d %v", count, err)
	}
	if _, err = db.ExecContext(t.Context(), "UPDATE schema_meta SET version=999"); err != nil {
		t.Fatal(err)
	}
	if reopened, _, err := openJournalCursorStore(t.Context(), path, time.Now()); err == nil {
		_ = reopened.Close()
		t.Fatal("newer cursor schema was silently opened")
	}
}

func TestJournalCatchupFirstEnableDoesNotUploadOlderHistory(t *testing.T) {
	root, spool := t.TempDir(), t.TempDir()
	old := time.Now().Add(-time.Hour)
	log, _, err := journal.OpenInstanceLog(filepath.Join(root, "scheduler"), journal.WithClock(func() time.Time { return old }))
	if err != nil {
		t.Fatal(err)
	}
	if err = log.Append(journal.Event{Type: journal.EventTickSkipped}); err != nil {
		t.Fatal(err)
	}
	_ = log.Close()
	receiver := &journalLoadReceiver{}
	client := newJournalLoadClient(t, receiver, func(cfg *Config) {
		cfg.JournalRoot, cfg.AzureMonitorReplayRoot = root, spool
	})
	log, _, err = journal.OpenInstanceLog(filepath.Join(root, "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	if err = log.Append(journal.Event{Type: journal.EventTickSkipped}); err != nil {
		t.Fatal(err)
	}
	_ = log.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err = client.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := settleJournalLoad(t, client).Accepted; got != 1 {
		t.Fatalf("first enable uploaded history: accepted=%d", got)
	}
}

func TestJournalCatchupCanceledFlushPreservesHints(t *testing.T) {
	exporter := &journalTestExporter{started: make(chan struct{}), release: make(chan struct{})}
	client := journalTestClient(t, exporter)
	rootPath, spool := t.TempDir(), t.TempDir()
	root, err := os.OpenRoot(rootPath)
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
	source := &journalCatchup{pipeline: client.journalLogs, root: rootPath, spool: spool, since: since, maxAge: time.Hour, hints: make(chan journalCatchupHint, 10)}
	log, _, err := journal.OpenInstanceLog(filepath.Join(rootPath, "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	if err = log.Append(journal.Event{Type: journal.EventTickSkipped}); err != nil {
		t.Fatal(err)
	}
	_ = log.Close()
	source.offer(journalCatchupHint{dir: "scheduler"})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- source.flushHints(ctx, root, db) }()
	waitJournalStarted(t, exporter.started)
	// Export is parked; a journal append must not wait on the exporter's lock.
	log, _, err = journal.OpenInstanceLog(filepath.Join(rootPath, "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	if err = log.Append(journal.Event{Type: journal.EventTickSkipped}); err != nil {
		t.Fatal(err)
	}
	_ = log.Close()
	cancel()
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("flush returned %v", err)
	}
	if len(source.hints) != 1 {
		t.Fatal("canceled flush consumed its retry hint")
	}
	close(exporter.release)
	ctx, cancel = context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err = source.flushHints(ctx, root, db); err != nil {
		t.Fatal(err)
	}
	cursor, err := loadJournalCursor(ctx, db, "scheduler")
	if err != nil || cursor.seq != 2 {
		t.Fatalf("retry failed: cursor=%+v err=%v", cursor, err)
	}
}
