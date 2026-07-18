package journal

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/api/validate"
)

func TestInstanceLogRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scheduler")
	log, report, err := OpenInstanceLog(dir, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("OpenInstanceLog: %v", err)
	}
	if report.Repaired {
		t.Fatalf("fresh log should not report repair: %+v", report)
	}
	for _, ev := range []Event{
		{Type: EventTriggerFired, Workflow: "nominate", Reason: "cron"},
		{Type: EventClaimAcquired, Name: "issue-8", RunID: testIdentity().RunID},
		{Type: EventRunStarted, Workflow: "nominate", RunID: testIdentity().RunID},
		{Type: EventTickSkipped, Workflow: "implement", Reason: "conditions: max-parallel"},
		{Type: EventClaimReleased, Name: "issue-8", RunID: testIdentity().RunID},
	} {
		if err := log.Append(ev); err != nil {
			t.Fatalf("Append %s: %v", ev.Type, err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	events, err := ReadInstanceLog(dir)
	if err != nil {
		t.Fatalf("ReadInstanceLog: %v", err)
	}
	if len(events) != 5 {
		t.Fatalf("want 5 events, got %d", len(events))
	}
	for i, ev := range events {
		if ev.Seq != uint64(i+1) {
			t.Errorf("event %d: seq=%d want %d", i, ev.Seq, i+1)
		}
	}
	if events[1].RunID != testIdentity().RunID || events[1].Name != "issue-8" {
		t.Errorf("claim.acquired fields not round-tripped: %+v", events[1])
	}
	if events[3].Reason != "conditions: max-parallel" {
		t.Errorf("tick.skipped reason not round-tripped: %+v", events[3])
	}

	// Reopening an existing, clean log resumes seq without re-truncating.
	log2, report2, err := OpenInstanceLog(dir, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if report2.Repaired || report2.LastSeq != 5 {
		t.Fatalf("reopen report wrong: %+v", report2)
	}
	if err := log2.Append(Event{Type: EventTriggerFired, Workflow: "nominate"}); err != nil {
		t.Fatalf("Append after reopen: %v", err)
	}
	_ = log2.Close()
	events, _ = ReadInstanceLog(dir)
	if len(events) != 6 || events[5].Seq != 6 {
		t.Fatalf("seq did not continue across reopen: %+v", events)
	}
}

func TestInstanceLogIndependentWritersAllocateUniqueSequence(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scheduler")
	first, _, err := OpenInstanceLog(dir, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("open first instance log: %v", err)
	}
	defer func() { _ = first.Close() }()
	second, _, err := OpenInstanceLog(dir, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("open second instance log: %v", err)
	}
	defer func() { _ = second.Close() }()

	for i, writer := range []*InstanceLog{first, second, first} {
		if err := writer.Append(Event{Type: EventTriggerFired, Workflow: "workflow"}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	events, err := ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3", len(events))
	}
	for i, event := range events {
		if want := uint64(i + 1); event.Seq != want {
			t.Fatalf("event %d seq = %d, want %d", i, event.Seq, want)
		}
	}
}

func TestInstanceLogAppendWaitsForJournalLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scheduler")
	instanceLog, _, err := OpenInstanceLog(dir, WithClock(fixedClock()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = instanceLog.Close() }()

	lock, err := acquireJournalLock(dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	appendDone := make(chan error, 1)
	go func() {
		appendDone <- instanceLog.Append(Event{Type: EventTriggerFired, Workflow: "workflow"})
	}()

	select {
	case err := <-appendDone:
		releaseJournalLock(lock)
		t.Fatalf("Append returned before journal lock was released: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	releaseJournalLock(lock)

	select {
	case err := <-appendDone:
		if err != nil {
			t.Fatalf("Append after lock release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Append did not proceed after journal lock was released")
	}
}

// TestInstanceLogConcurrentAppendsAllocateUniqueMonotonicSequence is #530's
// N-goroutine acceptance test (the maintainer ruling's explicit ask): N
// independent OpenInstanceLog calls — one per goroutine, mirroring N real
// subprocesses (the daemon plus backlog-query/pr-claim CLI invocations, all
// opening the SAME instance log concurrently) — each append exactly one
// event at the same time. TestInstanceLogIndependentWritersAllocateUniqueSequence
// above only interleaves two writers SEQUENTIALLY; this drives real
// concurrent contention on the flock, which is what the original bug (two
// events sharing seq:5) actually needed to reproduce.
func TestInstanceLogConcurrentAppendsAllocateUniqueMonotonicSequence(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scheduler")
	const writers = 25

	var wg sync.WaitGroup
	errs := make(chan error, writers)
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func() {
			defer wg.Done()
			log, _, err := OpenInstanceLog(dir, WithClock(fixedClock()))
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = log.Close() }()
			errs <- log.Append(Event{Type: EventTriggerFired, Workflow: "workflow"})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent open+append: %v", err)
		}
	}

	events, err := ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != writers {
		t.Fatalf("events = %d, want %d (a lost/dropped append)", len(events), writers)
	}
	seen := make(map[uint64]bool, writers)
	for _, ev := range events {
		if seen[ev.Seq] {
			t.Fatalf("duplicate seq %d among %d concurrent writers — exactly #530's original bug", ev.Seq, writers)
		}
		seen[ev.Seq] = true
		if ev.Seq < 1 || ev.Seq > uint64(writers) {
			t.Fatalf("seq %d out of the expected [1,%d] range — a gap or an out-of-order allocation", ev.Seq, writers)
		}
	}
}

// TestInstanceLogRecoversTornTail proves the instance journal gets the same
// crash-recovery guarantee as a run journal: a torn final write is discarded and
// a corrective repaired event is appended, on Open (not a separate Recover call,
// since the instance log is opened once for the daemon's lifetime).
func TestInstanceLogRecoversTornTail(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scheduler")
	log, _, err := OpenInstanceLog(dir, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("OpenInstanceLog: %v", err)
	}
	if err := log.Append(Event{Type: EventTriggerFired, Workflow: "nominate"}); err != nil {
		t.Fatal(err)
	}
	_ = log.Close()

	// Simulate a crash mid-append: truncate off the trailing newline of the last record.
	path := filepath.Join(dir, fileEvents)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data[:len(data)-1], 0o644); err != nil {
		t.Fatal(err)
	}

	log2, report, err := OpenInstanceLog(dir, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("OpenInstanceLog after torn write: %v", err)
	}
	defer func() { _ = log2.Close() }()
	if !report.Repaired || report.TornBytes == 0 {
		t.Fatalf("expected a repaired torn tail: %+v", report)
	}

	events, err := ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != EventRepaired {
		t.Fatalf("expected exactly one repaired event, got %+v", events)
	}
}

// TestInstanceLogEmittedBytesMatchSchema is the InstanceLog counterpart to
// TestEmittedBytesMatchSchema: it validates the instance journal's actual
// on-disk event bytes against journal-event.schema.json, so the four
// instance-only event types (and the workflow/runId/reason fields) can't
// silently drift from the checked-in contract, matching #8's established
// drift-guard pattern.
func TestInstanceLogEmittedBytesMatchSchema(t *testing.T) {
	v, err := validate.New()
	if err != nil {
		t.Fatalf("build validator: %v", err)
	}

	dir := filepath.Join(t.TempDir(), "scheduler")
	log, _, err := OpenInstanceLog(dir, WithClock(fixedClock()))
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range []Event{
		{Type: EventTriggerFired, Workflow: "nominate", Reason: "scheduled"},
		{Type: EventTriggerFired, Workflow: "nominate", Reason: "catch-up (missed 3)"},
		{Type: EventTickSkipped, Workflow: "implement", Reason: "conditions: max-parallel"},
		{Type: EventTickSkipped, Workflow: "implement", Reason: "conditions: budget"},
		{Type: EventRunStarted, Workflow: "nominate", RunID: testIdentity().RunID},
		{Type: EventRunFinished, Workflow: "nominate", RunID: testIdentity().RunID, Status: string(PhaseCompleted)},
		{Type: EventClaimAcquired, Name: "issue-8", RunID: testIdentity().RunID, Workflow: "curate"},
		{Type: EventClaimReleased, Name: "issue-8", RunID: testIdentity().RunID, Workflow: "curate"},
		{Type: EventConfigReloaded, Runner: map[string]any{"oldDigest": Digest([]byte("old")), "newDigest": Digest([]byte("new"))}},
		{Type: EventConfigReloadRejected, Error: &ErrorDetail{Code: "config_reload_rejected", Message: "invalid workflow"}, Runner: map[string]any{"oldDigest": Digest([]byte("old")), "newDigest": Digest([]byte("invalid"))}},
	} {
		if err := log.Append(ev); err != nil {
			t.Fatalf("Append %s: %v", ev.Type, err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, fileEvents))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	if len(lines) == 0 {
		t.Fatal("no instance log lines emitted")
	}
	for i, line := range lines {
		if err := v.ValidateJSON("journal-event.schema.json", line); err != nil {
			t.Errorf("instance log line %d fails schema: %v\n%s", i, err, line)
		}
	}
}

func TestInstanceLogScrubsBeforeWrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scheduler")
	reg, scrub := DefaultScrubber()
	reg.Register([]byte(canary))
	log, _, err := OpenInstanceLog(dir, WithScrubber(scrub), WithClock(fixedClock()))
	if err != nil {
		t.Fatal(err)
	}
	reason := "leak: " + canary + "; Authorization: Basic " + basicAuthCredential
	if err := log.Append(Event{Type: EventTickSkipped, Reason: reason}); err != nil {
		t.Fatal(err)
	}
	_ = log.Close()
	for _, secret := range []string{canary, basicAuthCredential} {
		if hits := filesContaining(t, dir, []byte(secret)); len(hits) > 0 {
			t.Fatalf("credential leaked into instance log: %v", hits)
		}
	}
	events, err := ReadInstanceLog(dir)
	if err != nil {
		t.Fatalf("ReadInstanceLog: %v", err)
	}
	if got := events[0].Reason; !strings.Contains(got, Redacted) {
		t.Fatalf("instance log reason was not redacted: %q", got)
	}
}
