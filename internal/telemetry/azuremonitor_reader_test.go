package telemetry

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestAzureReplayHealthWriterContentionDoesNotBlockAccounting(t *testing.T) {
	now := time.Now()
	s := testAzureReplaySpool(t, t.TempDir(), now)
	if err := s.submit(t.Context(), []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	before := s.stats()
	if !before.AccountingReady || before.PendingRecords != 1 {
		t.Fatalf("initial accounting: %+v", before)
	}
	// A durable writer may hold its connection while synchronizing a file.
	// Sampling the last committed manifest must not queue behind that writer.
	conn, err := s.index.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	stats, err := s.index.stats(ctx, s.stream, now)
	if err != nil {
		t.Fatalf("writer occupancy blocked bounded accounting: %v", err)
	}
	if !stats.AccountingReady || stats.PendingRecords != before.PendingRecords || stats.PendingBytes != before.PendingBytes || stats.PendingFiles != before.PendingFiles {
		t.Fatalf("writer occupancy changed committed accounting: before=%+v after=%+v", before, stats)
	}
}

func TestAzureReplayHealthReaderSeesCommittedStateOnly(t *testing.T) {
	now := time.Now()
	s := testAzureReplaySpool(t, t.TempDir(), now)
	if err := s.submit(t.Context(), []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	tx, err := s.index.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(t.Context(), `UPDATE totals SET records=999`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	stats, err := s.index.stats(ctx, s.stream, now)
	cancel()
	if err != nil || !stats.AccountingReady || stats.PendingRecords != 1 {
		t.Fatalf("reader blocked or exposed uncommitted totals: stats=%+v err=%v", stats, err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err = s.submit(t.Context(), []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	stats = s.stats()
	if !stats.AccountingReady || stats.PendingRecords != 2 || stats.PendingFiles != 2 {
		t.Fatalf("reader failed to observe subsequent committed publication: %+v", stats)
	}
}

func TestAzureReplayHealthReaderIsReadOnlyAndClosedWithIndex(t *testing.T) {
	s, err := newAzureReplaySpool(azureReplayConfig{dir: t.TempDir(), maxAge: time.Hour, maxBytes: 1 << 20}, func(context.Context, []byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.close(ctx)
	})
	if err := s.ensureIndex(); err != nil {
		t.Fatal(err)
	}
	if err := s.index.wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	reader, writer := s.index.statsDB, s.index.db
	if reader == nil || reader == writer || reader.Stats().MaxOpenConnections != 1 {
		t.Fatal("health reader must be a separate single-connection pool")
	}
	if _, err := reader.ExecContext(t.Context(), `INSERT INTO totals VALUES('test',0,0,0)`); err == nil {
		t.Fatal("health reader accepted a write")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := s.close(ctx); err != nil {
		t.Fatal(err)
	}
	for _, db := range []*sql.DB{reader, writer} {
		if err := db.PingContext(t.Context()); err == nil {
			t.Fatal("index release left a database pool open")
		}
	}
}

func TestAzureReplayHealthReaderFollowsSharedIndexLifetime(t *testing.T) {
	cfg := azureReplayConfig{dir: t.TempDir()}
	acquire := func() (*azureReplayIndex, func()) {
		t.Helper()
		x, _, err := acquireReplayIndex(cfg)
		if err != nil {
			t.Fatal(err)
		}
		released := false
		release := func() {
			t.Helper()
			if released {
				return
			}
			released = true
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := x.release(ctx); err != nil {
				t.Errorf("release index: %v", err)
			}
		}
		t.Cleanup(release)
		if err := x.wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		return x, release
	}
	first, releaseFirst := acquire()
	second, releaseSecond := acquire()
	if first != second {
		t.Fatal("same replay root did not share an index")
	}
	reader, writer := first.statsDB, first.db
	releaseFirst()
	for _, db := range []*sql.DB{reader, writer} {
		if err := db.PingContext(t.Context()); err != nil {
			t.Fatalf("nonfinal release closed a shared pool: %v", err)
		}
	}
	releaseSecond()
	for _, db := range []*sql.DB{reader, writer} {
		if err := db.PingContext(t.Context()); err == nil {
			t.Fatal("final release left a shared pool open")
		}
	}
	reopened, _ := acquire()
	if reopened == first || reopened.statsDB == reader || reopened.db == writer {
		t.Fatal("reopen reused a released index or pool")
	}
	stats, err := reopened.stats(t.Context(), "", time.Now())
	if err != nil || !stats.AccountingReady {
		t.Fatalf("reopened reader unavailable: stats=%+v err=%v", stats, err)
	}
}
