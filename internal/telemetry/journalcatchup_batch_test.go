package telemetry

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestJournalCatchupHintWindowDoesNotRestartOnArrival(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := &journalCatchup{hints: make(chan journalCatchupHint, 1024)}
		first := journalCatchupHint{dir: "run", identity: "a", seq: 1, bytes: 10}
		started := time.Now()
		done := make(chan []journalCatchupHint, 1)
		go func() { done <- source.collectHints(t.Context(), first) }()
		synctest.Wait()
		time.Sleep(90 * time.Millisecond)
		first.seq = 2
		source.hints <- first
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("sparse hints skipped their coalescing window")
		default:
		}
		got := <-done
		if time.Since(started) != journalLogBatchDelay || len(got) != 1 || got[0].seq != 2 {
			t.Fatalf("elapsed=%s hints=%+v", time.Since(started), got)
		}
	})
}

func TestJournalCatchupHintFullInputsAndBacklogDoNotWait(t *testing.T) {
	for _, test := range []struct {
		name  string
		count int
		bytes int
		more  bool
	}{
		{"record limit", journalLogBatchLimit, 1, false},
		{"byte limit", 8, journalLogBatchBytes / 8, false},
		{"large singleton", 1, journalLogBatchBytes + 1, false},
		{"retained continuation", 1, 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				source := &journalCatchup{hints: make(chan journalCatchupHint, 1024)}
				first := journalCatchupHint{dir: "run", identity: "a", seq: 1, bytes: test.bytes, resume: test.more}
				for seq := 2; seq <= test.count; seq++ {
					next := first
					next.seq = uint64(seq)
					source.hints <- next
				}
				started := time.Now()
				got := source.collectHints(t.Context(), first)
				if !time.Now().Equal(started) || len(got) != 1 || got[0].seq != uint64(test.count) {
					t.Fatalf("full input waited %s: %+v", time.Since(started), got)
				}
			})
		})
	}
}

func TestJournalCatchupHintCancelReturnsCollectedWatermarks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		source := &journalCatchup{hints: make(chan journalCatchupHint, 1024)}
		done := make(chan []journalCatchupHint, 1)
		go func() { done <- source.collectHints(ctx, journalCatchupHint{dir: "run", seq: 1}) }()
		synctest.Wait()
		source.hints <- journalCatchupHint{dir: "run", seq: 2}
		synctest.Wait()
		started := time.Now()
		cancel()
		got := <-done
		if !time.Now().Equal(started) || len(got) != 1 || got[0].seq != 2 {
			t.Fatalf("cancel delayed or lost collected watermark: %+v", got)
		}
	})
}

func TestJournalCatchupHintFlushDoesNotWaitAndHonorsReplacement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := &journalCatchup{hints: make(chan journalCatchupHint, 1024)}
		source.hints <- journalCatchupHint{dir: "run", identity: "a", seq: 9}
		source.hints <- journalCatchupHint{dir: "run", identity: "b", seq: 1}
		source.hints <- journalCatchupHint{dir: "second", identity: "c", seq: 4}
		started := time.Now()
		got := source.coalesceHints(journalCatchupHint{dir: "run", identity: "a", seq: 10})
		if !time.Now().Equal(started) || len(got) != 2 || got[0].identity != "b" || got[0].seq != 1 || got[1].seq != 4 {
			t.Fatalf("flush coalescing delayed or lost replacement: %+v", got)
		}
	})
}

func TestJournalCatchupHintBatchIsBoundedUnderQueuedFlood(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := &journalCatchup{hints: make(chan journalCatchupHint, 1024)}
		for seq := 2; seq <= cap(source.hints)+1; seq++ {
			source.hints <- journalCatchupHint{dir: "run", identity: "a", seq: uint64(seq)}
		}
		got := source.collectHints(t.Context(), journalCatchupHint{dir: "run", identity: "a", seq: 1})
		if len(got) != 1 || got[0].seq != journalLogBatchLimit || len(source.hints) != 1024-journalLogBatchLimit+1 {
			t.Fatalf("unbounded collection or lost queued hints: %+v, pending=%d", got, len(source.hints))
		}
	})
}

func TestJournalCatchupHintChargesInputAndResumesActualBacklog(t *testing.T) {
	rootPath, spool := t.TempDir(), t.TempDir()
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	since := time.Now().Add(-time.Minute)
	db, _, err := openJournalCursorStore(t.Context(), spool, since)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	client := journalTestClient(t, &journalTestExporter{})
	source := &journalCatchup{pipeline: client.journalLogs, root: rootPath, spool: spool,
		since: since, maxAge: time.Hour, hints: make(chan journalCatchupHint, 1024)}
	event := journal.CommittedEvent{Kind: "scheduler", JournalID: "scheduler", Seq: 1, Body: make([]byte, 32<<10)}
	source.notify(event)
	notified := <-source.hints
	if notified.bytes != journalLogSize(event) || notified.bytes <= len(event.Body) || notified.resume {
		t.Fatalf("notification omitted charged input or invented a continuation: %+v", notified)
	}
	log, _, err := journal.OpenInstanceLog(filepath.Join(rootPath, "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	for range 129 {
		if err := log.Append(journal.Event{Type: journal.EventTickSkipped}); err != nil {
			t.Fatal(err)
		}
	}
	source.process(t.Context(), root, db, journalCatchupHint{dir: "scheduler", seq: 129})
	if len(source.hints) != 1 {
		t.Fatalf("full retained batch did not schedule continuation: %d", len(source.hints))
	}
	next := <-source.hints
	if !next.resume || next.seq != 129 {
		t.Fatalf("retained continuation would pay a sparse delay: %+v", next)
	}
	source.process(t.Context(), root, db, next)
	cursor, err := loadJournalCursor(t.Context(), db, "scheduler")
	if err != nil || cursor.seq != 129 || len(source.hints) != 0 || client.journalLogs.accepted.Load() != 129 {
		t.Fatalf("continuation did not reconcile: cursor=%+v pending=%d accepted=%d err=%v", cursor, len(source.hints), client.journalLogs.accepted.Load(), err)
	}
}
