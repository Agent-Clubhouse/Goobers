package rollup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSchedulerStreamBoundsBacklogHeap(t *testing.T) {
	const childEnv = "GOOBERS_TEST_SCHEDULER_BACKLOG_HEAP"
	if os.Getenv(childEnv) != "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestSchedulerStreamBoundsBacklogHeap$", "-test.v")
		cmd.Env = append(os.Environ(), childEnv+"=1")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("bounded-backlog subprocess: %v\n%s", err, output)
		}
		t.Logf("%s", output)
		return
	}

	root := t.TempDir()
	schedulerDir := filepath.Join(root, "scheduler")
	if err := os.MkdirAll(schedulerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(schedulerDir, fileEvents)
	writeLargeSchedulerBacklog(t, path)
	db := openTestDB(t, root)
	// Isolate the soft Go heap cap in a subprocess so concurrent package tests
	// cannot change the result. A cap alone is not an assertion: Go can exceed
	// it to retain live objects, so also measure peak allocated heap explicitly.
	previous := debug.SetMemoryLimit(32 << 20)
	defer debug.SetMemoryLimit(previous)
	runtime.GC()
	var peak atomic.Uint64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			var stats runtime.MemStats
			runtime.ReadMemStats(&stats)
			if stats.HeapAlloc > peak.Load() {
				peak.Store(stats.HeapAlloc)
			}
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	}()
	err := db.rebuildSchedulerLog(t.Context(), schedulerDir)
	close(stop)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("backlog_bytes=%d peak_heap_bytes=%d heap_limit_bytes=%d", info.Size(), peak.Load(), 32<<20)
	if peak.Load() > 80<<20 {
		t.Fatalf("scheduler backlog retained %d heap bytes; want <80 MiB for a >128 MiB backlog", peak.Load())
	}
	if got := len(schedulerEventTypes(t, db)); got != 64 {
		t.Fatalf("ingested %d events, want 64", got)
	}
	if offset, seq, _ := schedulerCursorRow(t, db); offset != info.Size() || seq != 128 {
		t.Fatalf("cursor = %d/%d, want %d/128", offset, seq, info.Size())
	}
}

func writeLargeSchedulerBacklog(t *testing.T, path string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	message := strings.Repeat("x", 2<<20)
	for seq := 1; seq <= 64; seq++ {
		line := instanceEventLine(seq*2-1, "fixture.ignored", fmt.Sprintf(`"error":{"code":"fixture","message":"%s"}`, message))
		// Unknown event types still decode their bounded payload, but do not
		// spend this memory regression exercising message-redaction CPU costs.
		line += "\n" + instanceEventLine(seq*2, "trigger.fired", `"workflow":"fixture"`)
		if _, err := fmt.Fprintln(file, line); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSchedulerStreamOversizedAndTornRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	tooLarge := strings.Repeat("x", maxSchedulerRecordBytes+1)
	body := "{\"seq\":1}\n" + tooLarge + "\n{\"seq\":2}\n" + tooLarge
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var sequences []uint64
	visit := func(event journalEvent) error {
		sequences = append(sequences, event.Seq)
		return nil
	}
	offset, _, skipped, err := readJSONLTail(t.Context(), path, 0, visit)
	if err != nil || skipped.count != 1 || len(sequences) != 2 || sequences[0] != 1 || sequences[1] != 2 || offset != int64(len(body)-len(tooLarge)) {
		t.Fatalf("read = %v, offset %d, skipped %+v, err %v", sequences, offset, skipped, err)
	}
	// Completing the oversized tail must skip it once and still ingest the
	// next valid record, without advancing across its torn prefix prematurely.
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := fmt.Fprint(file, "\n{\"seq\":3}\n")
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		t.Fatal(err)
	}
	_, _, skipped, err = readJSONLTail(t.Context(), path, offset, visit)
	if err != nil || skipped.count != 1 || len(sequences) != 3 || sequences[2] != 3 {
		t.Fatalf("resume = %v, skipped %+v, err %v", sequences, skipped, err)
	}
}

func TestSchedulerStreamCancellationBetweenRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte("{}\n{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	seen := 0
	_, _, _, err := readJSONLTail(ctx, path, 0, func(journalEvent) error {
		seen++
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || seen != 1 {
		t.Fatalf("seen=%d err=%v, want one record then cancellation", seen, err)
	}
}

func TestSchedulerStreamRollsBackOnLateUnknownSchema(t *testing.T) {
	root := t.TempDir()
	schedulerDir := filepath.Join(root, "scheduler")
	unknown := strings.Replace(instanceEventLine(6, "run.finished", `"status":"completed"`), eventSchema, "future/schema", 1)
	if err := writeInstanceEvents(t, schedulerDir, append(firstFive(), unknown)); err != nil {
		t.Fatal(err)
	}
	db := openTestDB(t, root)
	if err := db.IngestSchedulerLog(t.Context(), schedulerDir); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("late schema failure = %v", err)
	}
	if got := schedulerEventTypes(t, db); len(got) != 0 {
		t.Fatalf("partial rows survived rollback: %v", got)
	}
	cursor, err := readSchedulerCursor(t.Context(), db.sql)
	if err != nil || cursor.byteOffset != 0 || cursor.lastSeq != 0 {
		t.Fatalf("cursor advanced across rollback: %+v, %v", cursor, err)
	}
}

func TestSchedulerStreamRollsBackEventsWhenSpansFail(t *testing.T) {
	root := t.TempDir()
	schedulerDir := filepath.Join(root, "scheduler")
	if err := writeInstanceEvents(t, schedulerDir, firstFive()); err != nil {
		t.Fatal(err)
	}
	// Events have already been projected into the transaction by the time the
	// span stream is opened. A span I/O failure must roll those rows back too.
	if err := os.MkdirAll(filepath.Join(schedulerDir, dirSpans, fileSpans), 0o700); err != nil {
		t.Fatal(err)
	}
	db := openTestDB(t, root)
	if err := db.IngestSchedulerLog(t.Context(), schedulerDir); err == nil {
		t.Fatal("ingesting a directory as the span stream succeeded")
	}
	if got := schedulerEventTypes(t, db); len(got) != 0 {
		t.Fatalf("partial rows survived rollback: %v", got)
	}
	cursor, err := readSchedulerCursor(t.Context(), db.sql)
	if err != nil || cursor.byteOffset != 0 || cursor.lastSeq != 0 {
		t.Fatalf("cursor advanced across rollback: %+v, %v", cursor, err)
	}
}
