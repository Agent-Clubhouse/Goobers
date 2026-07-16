package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRecoverSerializesConcurrentWriters is #243's core acceptance test: two
// writers opening the same run directory (e.g. `goobers run abort` racing a
// live daemon's own Resume of a crashed run) must never both hold an
// independent *Run over one events.jsonl at the same time. The second
// Recover call blocks until the first releases its lock via Close, rather
// than proceeding immediately and risking interleaved appends.
func TestRecoverSerializesConcurrentWriters(t *testing.T) {
	root := t.TempDir()
	run, err := Create(root, testIdentity(), nil, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	dir := filepath.Join(root, testIdentity().RunID)

	first, _, err := Recover(dir, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("first Recover: %v", err)
	}

	second := make(chan struct{})
	go func() {
		r2, _, err := Recover(dir, WithClock(fixedClock()))
		if err != nil {
			t.Errorf("second Recover: %v", err)
			close(second)
			return
		}
		_ = r2.Close()
		close(second)
	}()

	// The second Recover must still be blocked shortly after being started
	// — it must NOT have proceeded while the first writer still holds the
	// lock.
	select {
	case <-second:
		t.Fatal("second Recover returned before the first Close — the lock did not serialize the two writers")
	case <-time.After(200 * time.Millisecond):
	}

	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	select {
	case <-second:
	case <-time.After(5 * time.Second):
		t.Fatal("second Recover did not proceed after the first released its lock")
	}
}

// TestRecoverRefreshesEventsAfterWaitingForLock covers #277: the log snapshot
// taken before a blocking lock acquisition may contain a torn tail that the
// lock holder completes before releasing. Recovery must not truncate the newer
// tail using the stale torn-byte count or resume from a stale sequence.
func TestRecoverRefreshesEventsAfterWaitingForLock(t *testing.T) {
	root := t.TempDir()
	run, err := Create(root, testIdentity(), nil, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	dir := filepath.Join(root, testIdentity().RunID)
	eventsPath := filepath.Join(dir, fileEvents)
	lock, err := acquireRunLock(dir)
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	lockHeld := true
	defer func() {
		if lockHeld {
			releaseRunLock(lock)
		}
	}()

	events, _, err := readEvents(eventsPath)
	if err != nil {
		t.Fatalf("readEvents: %v", err)
	}
	inFlight, err := json.Marshal(Event{
		Schema:  EventSchema,
		Seq:     uint64(len(events) + 1),
		Type:    EventStageStarted,
		Time:    fixedClock()(),
		Stage:   "completed-while-waiting",
		Attempt: 1,
	})
	if err != nil {
		t.Fatalf("marshal in-flight event: %v", err)
	}
	later, err := json.Marshal(Event{
		Schema:  EventSchema,
		Seq:     uint64(len(events) + 2),
		Type:    EventStageStarted,
		Time:    fixedClock()(),
		Stage:   "appended-while-waiting",
		Attempt: 1,
	})
	if err != nil {
		t.Fatalf("marshal later event: %v", err)
	}

	log, err := os.OpenFile(eventsPath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open events log: %v", err)
	}
	split := len(inFlight) / 2
	if _, err := log.Write(inFlight[:split]); err != nil {
		_ = log.Close()
		t.Fatalf("write torn event prefix: %v", err)
	}
	if err := log.Sync(); err != nil {
		_ = log.Close()
		t.Fatalf("sync torn event prefix: %v", err)
	}

	type recoverResult struct {
		run    *Run
		report RecoverReport
		err    error
	}
	recovered := make(chan recoverResult, 1)
	go func() {
		r, report, err := Recover(dir, WithClock(fixedClock()))
		recovered <- recoverResult{run: r, report: report, err: err}
	}()

	select {
	case result := <-recovered:
		if result.run != nil {
			_ = result.run.Close()
		}
		t.Fatal("Recover returned while the run lock was held")
	case <-time.After(200 * time.Millisecond):
	}

	completedTail := append([]byte{}, inFlight[split:]...)
	completedTail = append(completedTail, '\n')
	completedTail = append(completedTail, later...)
	completedTail = append(completedTail, '\n')
	if _, err := log.Write(completedTail); err != nil {
		_ = log.Close()
		t.Fatalf("complete events while Recover waits: %v", err)
	}
	if err := log.Sync(); err != nil {
		_ = log.Close()
		t.Fatalf("sync completed events: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("close events log: %v", err)
	}
	releaseRunLock(lock)
	lockHeld = false

	var result recoverResult
	select {
	case result = <-recovered:
	case <-time.After(5 * time.Second):
		t.Fatal("Recover did not proceed after the run lock was released")
	}
	if result.err != nil {
		t.Fatalf("Recover: %v", result.err)
	}
	if result.report.TornBytes != 0 || result.report.Repaired {
		_ = result.run.Close()
		t.Fatalf("Recover used stale torn-tail state: %+v", result.report)
	}
	if result.report.LastSeq != uint64(len(events)+2) {
		_ = result.run.Close()
		t.Fatalf("LastSeq=%d want %d", result.report.LastSeq, len(events)+2)
	}
	if err := result.run.Append(Event{Type: EventStageStarted, Stage: "after-recover", Attempt: 1}); err != nil {
		_ = result.run.Close()
		t.Fatalf("Append after Recover: %v", err)
	}
	if err := result.run.Close(); err != nil {
		t.Fatalf("Close recovered run: %v", err)
	}

	rd, err := OpenRead(dir)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	got, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(got) != len(events)+3 {
		t.Fatalf("event count=%d want %d", len(got), len(events)+3)
	}
	for i, event := range got {
		if event.Seq != uint64(i+1) {
			t.Fatalf("non-contiguous seq at %d: %d", i, event.Seq)
		}
	}
}

// TestCreateSerializesAgainstConcurrentRecover confirms the lock is
// symmetric: a Recover holding the run dir must block a concurrent Create
// attempt on a run id that Recover is still resuming... actually Create
// only ever targets a brand-new id (Mkdir's own EEXIST already refuses a
// second Create of the same id), so this test instead confirms Create
// itself acquires and later releases the lock — a subsequent Recover must
// be able to proceed once Create's own Run is closed, not find the lock
// file left stuck held.
func TestCreateReleasesLockOnClose(t *testing.T) {
	root := t.TempDir()
	run, err := Create(root, testIdentity(), nil, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	dir := filepath.Join(root, testIdentity().RunID)

	done := make(chan struct{})
	go func() {
		r2, _, err := Recover(dir, WithClock(fixedClock()))
		if err != nil {
			t.Errorf("Recover after Create+Close: %v", err)
			close(done)
			return
		}
		_ = r2.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Recover did not proceed after Create released its lock on Close — lock leaked")
	}
}
