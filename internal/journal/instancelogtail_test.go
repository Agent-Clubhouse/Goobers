package journal

import (
	"os"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/readprobe"
)

func TestInstanceLogTailReadsOnlyNewBytes(t *testing.T) {
	dir := t.TempDir()
	history := make([]string, 2000)
	for i := range history {
		history[i] = eventLine(i+1, fixedClock()(), "")
	}
	writeRawInstanceLog(t, dir, history...)
	log, _, err := OpenInstanceLog(dir, WithClock(fixedClock()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	tail, err := OpenInstanceLogTail(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tail.Close() }()
	path, err := InstanceEventsPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := log.Append(Event{Type: EventRunStarted, RunID: "new"}); err != nil {
		t.Fatal(err)
	}
	readprobe.Enable()
	t.Cleanup(readprobe.Disable)
	events, err := tail.Events()
	if err != nil {
		t.Fatal(err)
	}
	work := readprobe.Take()
	if len(events) != 1 || events[0].RunID != "new" {
		t.Fatalf("events = %#v, want only new run", events)
	}
	if work.InstanceTailReads != 1 || work.InstanceTailBytes == 0 || work.InstanceTailBytes > 1024 {
		t.Fatalf("tail work = %+v for %d-byte history", work, info.Size())
	}
}

func TestInstanceLogTailFollowsCompactionWithoutLossOrDuplicates(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	log, _, err := OpenInstanceLog(dir, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	if err := log.Append(Event{Type: EventTickSkipped, Reason: "drop"}); err != nil {
		t.Fatal(err)
	}
	if err := log.Append(Event{Type: EventTriggerFired, Workflow: "keep"}); err != nil {
		t.Fatal(err)
	}
	tail, err := OpenInstanceLogTail(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tail.Close() }()

	now = now.Add(time.Hour)
	if err := log.Append(Event{Type: EventRunStarted, RunID: "before-rotation"}); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Compact(now.Add(-time.Minute), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := log.Append(Event{Type: EventRunFinished, RunID: "after-rotation"}); err != nil {
		t.Fatal(err)
	}

	events, err := tail.Events()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 ||
		events[0].RunID != "before-rotation" ||
		events[1].RunID != "after-rotation" {
		t.Fatalf("events across rotation = %#v", events)
	}
	events, err = tail.Events()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("replayed events after rotation = %#v", events)
	}
}
