package journal

import (
	"os"
	"testing"
	"time"
)

func TestInstanceLogCursorReadsOnlyAppendsAndResetsOnReplacement(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	log, _, err := OpenInstanceLog(dir, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	appendReasons := func(reasons ...string) {
		t.Helper()
		for _, reason := range reasons {
			if err := log.Append(Event{Type: EventTickSkipped, Reason: reason}); err != nil {
				t.Fatal(err)
			}
		}
	}
	next := func(cursor *InstanceLogCursor, wantReset bool, want ...string) {
		t.Helper()
		events, reset, err := cursor.Next()
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, event := range events {
			got = append(got, event.Reason)
		}
		if reset != wantReset || len(got) != len(want) {
			t.Fatalf("Next() = %q reset=%v, want %q reset=%v", got, reset, want, wantReset)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("Next() = %q, want %q", got, want)
			}
		}
	}

	cursor := NewInstanceLogCursor(dir)
	appendReasons("a", "b")
	next(cursor, true, "a", "b")
	next(cursor, false)
	appendReasons("c")
	next(cursor, false, "c")

	// Compaction preserves sequences but replaces the generation: the cursor
	// must hand back the replacement in full so the caller can rebuild.
	now = now.Add(time.Hour)
	appendReasons("d")
	if _, err := log.Compact(now.Add(-time.Minute), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	next(cursor, true, "d")
	appendReasons("e")
	next(cursor, false, "e")

	// A recreated journal restarts its sequence; reading after the old
	// watermark would miss everything in it.
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	path, err := InstanceEventsPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	appendInstanceEvents(t, dir, Event{Type: EventTickSkipped, Reason: "f"})
	next(cursor, true, "f")
}

func TestInstanceLogCursorToleratesMissingJournal(t *testing.T) {
	dir := t.TempDir()
	cursor := NewInstanceLogCursor(dir)
	events, reset, err := cursor.Next()
	if err != nil || !reset || len(events) != 0 {
		t.Fatalf("Next() on a missing journal = %v reset=%v err=%v, want empty reset", events, reset, err)
	}
	appendInstanceEvents(t, dir, Event{Type: EventDaemonStarted})
	events, reset, err = cursor.Next()
	if err != nil || !reset || len(events) != 1 {
		t.Fatalf("Next() after creation = %v reset=%v err=%v, want one event and reset", events, reset, err)
	}
}

func TestInstanceLogLastSeqIsAnAfterSeqWatermark(t *testing.T) {
	dir := t.TempDir()
	if seq, err := InstanceLogLastSeq(dir); err != nil || seq != 0 {
		t.Fatalf("InstanceLogLastSeq(missing) = %d, %v; want 0", seq, err)
	}
	appendInstanceEvents(t, dir, Event{Type: EventDaemonStarted}, Event{Type: EventTickSkipped})
	seq, err := InstanceLogLastSeq(dir)
	if err != nil {
		t.Fatal(err)
	}
	appendInstanceEvents(t, dir, Event{Type: EventTelemetryRetentionPass})
	events, err := ReadInstanceLogAfterSeq(dir, seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != EventTelemetryRetentionPass {
		t.Fatalf("events after watermark %d = %+v, want only the later append", seq, events)
	}
}
