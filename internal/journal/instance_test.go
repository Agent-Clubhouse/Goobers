package journal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

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

func TestInstanceLogSerializesIndependentWriters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scheduler")
	first, _, err := OpenInstanceLog(dir, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("OpenInstanceLog first writer: %v", err)
	}
	defer func() { _ = first.Close() }()
	second, _, err := OpenInstanceLog(dir, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("OpenInstanceLog second writer: %v", err)
	}
	defer func() { _ = second.Close() }()

	const perWriter = 50
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i, log := range []*InstanceLog{first, second} {
		wg.Add(1)
		go func(writer int, log *InstanceLog) {
			defer wg.Done()
			for n := 0; n < perWriter; n++ {
				if err := log.Append(Event{Type: EventTriggerFired, Workflow: fmt.Sprintf("writer-%d", writer)}); err != nil {
					errs <- err
					return
				}
			}
		}(i, log)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Append: %v", err)
	}

	events, err := ReadInstanceLog(dir)
	if err != nil {
		t.Fatalf("ReadInstanceLog: %v", err)
	}
	if len(events) != 2*perWriter {
		t.Fatalf("events = %d, want %d", len(events), 2*perWriter)
	}
	for i, ev := range events {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("event %d seq = %d, want %d", i, ev.Seq, i+1)
		}
	}
}

func TestInstanceLogAppendPreservesLegacySequenceHighWaterMark(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scheduler")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
	for _, seq := range []uint64{1, 3, 2} {
		if err := encoder.Encode(Event{Seq: seq, Type: EventTriggerFired, Workflow: "legacy"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, fileEvents), data.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	log, report, err := OpenInstanceLog(dir, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("OpenInstanceLog: %v", err)
	}
	defer func() { _ = log.Close() }()
	if report.LastSeq != 3 {
		t.Fatalf("last seq = %d, want legacy high-water mark 3", report.LastSeq)
	}
	if err := log.Append(Event{Type: EventTriggerFired, Workflow: "current"}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	events, err := ReadInstanceLog(dir)
	if err != nil {
		t.Fatalf("ReadInstanceLog: %v", err)
	}
	last := events[len(events)-1]
	if last.Seq != 4 || last.Workflow != "current" {
		t.Fatalf("last event = %+v, want seq 4 current event", last)
	}
}

func TestInstanceLogAppendSkipsCorruptCompletedEvent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scheduler")
	log, _, err := OpenInstanceLog(dir, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("OpenInstanceLog: %v", err)
	}
	defer func() { _ = log.Close() }()

	if err := log.Append(Event{Type: EventTriggerFired, Workflow: "nominate"}); err != nil {
		t.Fatalf("Append initial event: %v", err)
	}
	path := filepath.Join(dir, fileEvents)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{corrupt completed event}\n"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if err := log.Append(Event{Type: EventTriggerFired, Workflow: "implement"}); err != nil {
		t.Fatalf("Append after corrupt event: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	var last Event
	if err := json.Unmarshal(lines[len(lines)-1], &last); err != nil {
		t.Fatalf("decode last event: %v", err)
	}
	if last.Seq != 2 || last.Workflow != "implement" {
		t.Fatalf("last event = %+v, want seq 2 implement event", last)
	}
}

func TestInstanceLogRecoversTornTailOnAppend(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scheduler")
	log, _, err := OpenInstanceLog(dir, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("OpenInstanceLog: %v", err)
	}
	defer func() { _ = log.Close() }()

	if err := log.Append(Event{Type: EventTriggerFired, Workflow: "nominate"}); err != nil {
		t.Fatalf("Append initial event: %v", err)
	}
	path := filepath.Join(dir, fileEvents)
	torn := []byte(`{"seq":2,"type":"trigger.fired"`)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(torn); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if err := log.Append(Event{Type: EventTriggerFired, Workflow: "implement"}); err != nil {
		t.Fatalf("Append after torn tail: %v", err)
	}
	events, err := ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("events = %+v, want original, repair, and appended event", events)
	}
	if events[1].Seq != 2 || events[1].Type != EventRepaired ||
		events[1].Runner["discardedBytes"] != float64(len(torn)) {
		t.Fatalf("repair event = %+v, want %d discarded bytes", events[1], len(torn))
	}
	if events[2].Seq != 3 || events[2].Workflow != "implement" {
		t.Fatalf("appended event = %+v, want seq 3 implement event", events[2])
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
	if err := log.Append(Event{Type: EventTickSkipped, Reason: "leak: " + canary}); err != nil {
		t.Fatal(err)
	}
	_ = log.Close()
	if hits := filesContaining(t, dir, []byte(canary)); len(hits) > 0 {
		t.Fatalf("canary leaked into instance log: %v", hits)
	}
}
