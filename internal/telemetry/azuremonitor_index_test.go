package telemetry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestAzureReplayIndexCoalescesTinyFilesAndRetainsFailedBatch(t *testing.T) {
	s := testAzureReplaySpool(t, t.TempDir(), time.Now())
	for i := range 300 {
		if _, err := s.writeLocked(fmt.Sprintf("%020d.ndjson", i), s.now(), []byte(fmt.Sprintf("{\"id\":%d}", i))); err != nil {
			t.Fatal(err)
		}
	}
	var bodies [][]byte
	s.send = func(_ context.Context, body []byte) error {
		bodies = append(bodies, append([]byte(nil), body...))
		return errors.New("offline")
	}
	if err := s.drain(context.Background()); err == nil {
		t.Fatal("missing upload error")
	}
	if stats := s.stats(); stats.PendingRecords != 300 || stats.PendingFiles != 300 || stats.Delivered != 0 {
		t.Fatalf("stats=%+v", stats)
	}
	s.send = func(_ context.Context, body []byte) error {
		bodies = append(bodies, append([]byte(nil), body...))
		return nil
	}
	if err := s.drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 4 || string(bodies[0]) != string(bodies[1]) {
		t.Fatalf("batch retry was not stable, calls=%d", len(bodies))
	}
	for i, body := range bodies {
		if n := azureReplayRecordCount(body); n > 128 || n == 0 {
			t.Fatalf("batch %d records=%d", i, n)
		}
	}
	if stats := s.stats(); stats.PendingFiles != 0 || stats.Delivered != 300 {
		t.Fatalf("stats=%+v", stats)
	}
}

func TestAzureReplayIndexReconcilesInterruptedMutationAndExternalWriter(t *testing.T) {
	s := testAzureReplaySpool(t, t.TempDir(), time.Now())
	if err := s.submit(context.Background(), []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	err := s.index.withLock(context.Background(), func(_ *sql.Tx) error {
		_, err := s.writeLocked("external.ndjson", s.now(), []byte("{}\n{}\n"))
		if err != nil {
			return err
		}
		s.index.dirty = true // Model the production publication marker.
		return errors.New("simulate interrupted transaction after file publish")
	})
	if err == nil {
		t.Fatal("expected interrupted transaction")
	}
	if err = s.submit(context.Background(), []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	if stats := s.stats(); stats.PendingRecords != 4 || stats.PendingFiles != 3 {
		t.Fatalf("reconciled stats=%+v", stats)
	}
	if err = os.Remove(filepath.Join(s.cfg.dir, "external.ndjson")); err != nil {
		t.Fatal(err)
	}
	// Directory timestamps need not immediately expose an external delete on
	// Windows. The periodic audit guarantees recovery even with an unchanged
	// stamp; expire its persisted cadence deterministically rather than sleep.
	if _, err = s.index.db.ExecContext(t.Context(), `UPDATE reconciliation SET audited=0`); err != nil {
		t.Fatal(err)
	}
	if err = s.index.withLock(context.Background(), func(*sql.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if stats := s.stats(); stats.PendingRecords != 2 || stats.PendingFiles != 2 {
		t.Fatalf("external delete stats=%+v", stats)
	}
}

func TestAzureReplayIndexIndependentHandlesRespectClaims(t *testing.T) {
	s := testAzureReplaySpool(t, t.TempDir(), time.Now())
	if err := s.submit(context.Background(), []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	batch, err := s.claimBatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// A separate SQLite connection and OS-lock handle model another process,
	// deliberately bypassing the in-process shared-index registry.
	x := &azureReplayIndex{root: s.index.root, streams: []string{""}, ready: make(chan struct{})}
	if err = x.open(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(x.ready)
	t.Cleanup(func() { _ = x.closeDatabases() })
	other := &azureReplaySpool{cfg: s.cfg, index: x, stream: "", now: time.Now, wake: make(chan struct{}, 1)}
	if _, err = other.claimBatch(context.Background()); err == nil {
		t.Fatal("second connection claimed in-flight file")
	}
	if err = s.finishBatch(context.Background(), batch, false); err != nil {
		t.Fatal(err)
	}
	batch, err = other.claimBatch(context.Background())
	if err != nil || len(batch.files) != 1 {
		t.Fatalf("released claim unavailable: %v %+v", err, batch)
	}
	if err = other.finishBatch(context.Background(), batch, true); err != nil {
		t.Fatal(err)
	}
	if stats := s.stats(); stats.PendingRecords != 0 {
		t.Fatalf("shared totals stale: %+v", stats)
	}
}

func TestAzureReplayIndexAuditRecoversUnchangedDirectoryStamp(t *testing.T) {
	s := testAzureReplaySpool(t, t.TempDir(), time.Now())
	if err := s.submit(t.Context(), []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeLocked("coalesced.ndjson", s.now(), []byte("{}\n{}\n")); err != nil {
		t.Fatal(err)
	}
	stamp, err := directoryStamp(s.cfg.dir)
	if err != nil {
		t.Fatal(err)
	}
	// Model the directory stamp failing to reveal an external publication.
	if _, err = s.index.db.ExecContext(t.Context(), `UPDATE directories SET modified=?`, stamp); err != nil {
		t.Fatal(err)
	}
	if err = s.index.withLock(t.Context(), func(*sql.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if stats := s.stats(); stats.PendingRecords != 1 {
		t.Fatalf("unexpected hot-path full scan: %+v", stats)
	}
	if _, err = s.index.db.ExecContext(t.Context(), `UPDATE reconciliation SET audited=0`); err != nil {
		t.Fatal(err)
	}
	if err = s.index.withLock(t.Context(), func(*sql.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if stats := s.stats(); stats.PendingRecords != 3 {
		t.Fatalf("audit failed to find hidden publication: %+v", stats)
	}
	var audited int64
	if err = s.index.db.QueryRowContext(t.Context(), `SELECT audited FROM reconciliation`).Scan(&audited); err != nil {
		t.Fatal(err)
	}
	other := &azureReplayIndex{root: s.index.root, streams: []string{""}}
	if err = other.open(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.closeDatabases() }()
	var reopened int64
	if err = other.db.QueryRowContext(t.Context(), `SELECT audited FROM reconciliation`).Scan(&reopened); err != nil {
		t.Fatal(err)
	}
	if audited == 0 || reopened != audited {
		t.Fatal("short-lived opener repeated the full audit")
	}
}

func TestAzureReplayIndexV1AuditMigrationPreservesPayloads(t *testing.T) {
	s := testAzureReplaySpool(t, t.TempDir(), time.Now())
	if err := s.submit(t.Context(), []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	// Recreate the preceding schema shape in this isolated fixture, retaining
	// manifest rows and authoritative files; opening must migrate, not reset.
	if _, err := s.index.db.ExecContext(t.Context(), `DROP TABLE reconciliation; UPDATE schema_meta SET version=1`); err != nil {
		t.Fatal(err)
	}
	other := &azureReplayIndex{root: s.index.root, streams: []string{""}}
	if err := other.open(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.closeDatabases() }()
	var version int
	if err := other.db.QueryRowContext(t.Context(), `SELECT version FROM schema_meta`).Scan(&version); err != nil || version != len(replayIndexMigrations) {
		t.Fatalf("version=%d err=%v", version, err)
	}
	if stats := s.stats(); stats.PendingRecords != 1 || stats.PendingFiles != 1 {
		t.Fatalf("migration lost payload: %+v", stats)
	}
}

func TestAzureReplayIndexExpiredCrashLeaseBecomesEligible(t *testing.T) {
	s := testAzureReplaySpool(t, t.TempDir(), time.Now())
	if err := s.submit(context.Background(), []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.claimBatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := s.index.withLock(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE files SET lease_until=?`, time.Now().Add(-time.Second).UnixNano())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	batch, err := s.claimBatch(context.Background())
	if err != nil || len(batch.files) != 1 {
		t.Fatalf("expired claim not recovered: %v", err)
	}
}

func TestAzureReplayIndexBatchByteBound(t *testing.T) {
	s := testAzureReplaySpool(t, t.TempDir(), time.Now())
	s.cfg.maxBytes = 8 << 20
	payload := []byte(`{"padding":"` + strings.Repeat("x", 600<<10) + "\"}\n")
	for i := range 3 {
		if _, err := s.writeLocked(fmt.Sprintf("%020d.ndjson", i), s.now(), payload); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	s.send = func(_ context.Context, body []byte) error {
		calls++
		if len(body) > azureReplayBatchBytes {
			t.Errorf("combined upload too large: %d", len(body))
		}
		return nil
	}
	if err := s.drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestAzureReplayIndexRecoversAfterTransientInitializationFailure(t *testing.T) {
	dir := t.TempDir()
	blocked := filepath.Join(dir, azureReplayIndexName)
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	s := testAzureReplaySpool(t, dir, time.Now())
	if stats := s.stats(); stats.AccountingReady {
		t.Fatal("failed initialization reported healthy")
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for !s.stats().AccountingReady {
		if !waitJournalCatchup(ctx, 10*time.Millisecond) {
			t.Fatal("manifest never recovered")
		}
	}
	if err := s.submit(ctx, []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	if stats := s.stats(); !stats.AccountingReady || stats.PendingRecords != 1 {
		t.Fatalf("did not recover: %+v", stats)
	}
}

func TestAzureReplaySuccessfulWorkBudgetYieldsWithoutOutageBackoff(t *testing.T) {
	s := testAzureReplaySpool(t, t.TempDir(), time.Now())
	for range 2 {
		if err := s.submit(context.Background(), []byte(strings.Repeat("{}\n", 128))); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.send = func(context.Context, []byte) error { cancel(); return nil }
	err := s.drain(ctx)
	if !errors.Is(err, errAzureReplayYield) {
		t.Fatalf("successful time-sliced work treated as outage: %v", err)
	}
	if stats := s.stats(); stats.Delivered != 128 || stats.PendingRecords != 128 || stats.Retried != 0 {
		t.Fatalf("stats=%+v", stats)
	}
	s.send = func(context.Context, []byte) error { return nil }
	if err = s.drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stats := s.stats(); stats.PendingRecords != 0 {
		t.Fatalf("continuation left records: %+v", stats)
	}
}

func TestAzureReplayIndexLocalLockWaitIsCancelable(t *testing.T) {
	s := testAzureReplaySpool(t, t.TempDir(), time.Now())
	if err := s.ensureIndex(); err != nil {
		t.Fatal(err)
	}
	if err := s.index.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	unlock, err := s.index.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.index.lock(ctx)
	unlock()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting producer ignored cancellation: %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	unlock, err = s.index.lock(ctx)
	if err != nil {
		t.Fatalf("canceled waiter leaked permit: %v", err)
	}
	unlock()
}

func TestAzureReplayIndexWaitingAdmissionPrecedesHotDrainer(t *testing.T) {
	root := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		x := &azureReplayIndex{root: root}
		unlock, err := x.lock(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		order := make(chan string, 2)
		go func() {
			release, err := x.lock(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			order <- "admission"
			release()
		}()
		// The producer is now durably waiting, not dependent on a wall-clock
		// sleep or the host scheduler being fast enough to start its goroutine.
		synctest.Wait()
		unlock()
		unlock, err = x.lock(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		order <- "drainer"
		unlock()
		if first := <-order; first != "admission" {
			t.Fatalf("hot drainer bypassed waiting producer: %s", first)
		}
	})
}

func BenchmarkAzureReplayIndexedStats(b *testing.B) {
	for _, count := range []int{0, 12000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			s := testAzureReplaySpool(b, b.TempDir(), time.Now())
			fixture := []byte(fmt.Sprintf("{\"schema\":%q,\"createdAt\":%q,\"records\":1}\n{}\n", azureReplaySchema, s.now().UTC().Format(time.RFC3339Nano)))
			for i := range count {
				if err := os.WriteFile(filepath.Join(s.cfg.dir, fmt.Sprintf("%020d.ndjson", i)), fixture, 0o600); err != nil {
					b.Fatal(err)
				}
			}
			if err := s.ensureIndex(); err != nil {
				b.Fatal(err)
			}
			if err := s.index.wait(context.Background()); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if stats := s.stats(); stats.PendingRecords != count || !stats.AccountingReady {
					b.Fatalf("stats=%+v", stats)
				}
			}
		})
	}
}

// Measure the periodic metadata audit separately from the O(1) stats path.
// Fixture publication and initial manifest construction are outside timing.
func BenchmarkAzureReplayIndexAudit(b *testing.B) {
	for _, count := range []int{0, 12000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			s := testAzureReplaySpool(b, b.TempDir(), time.Now())
			fixture := []byte(fmt.Sprintf("{\"schema\":%q,\"createdAt\":%q,\"records\":1}\n{}\n", azureReplaySchema, s.now().UTC().Format(time.RFC3339Nano)))
			for i := range count {
				if err := os.WriteFile(filepath.Join(s.cfg.dir, fmt.Sprintf("%020d.ndjson", i)), fixture, 0o600); err != nil {
					b.Fatal(err)
				}
			}
			if err := s.ensureIndex(); err != nil {
				b.Fatal(err)
			}
			if err := s.index.wait(b.Context()); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := s.index.db.ExecContext(b.Context(), `UPDATE reconciliation SET audited=0`); err != nil {
					b.Fatal(err)
				}
				if err := s.index.withLock(b.Context(), func(*sql.Tx) error { return nil }); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if stats := s.stats(); stats.PendingRecords != count {
				b.Fatalf("audit changed records: %+v", stats)
			}
		})
	}
}
