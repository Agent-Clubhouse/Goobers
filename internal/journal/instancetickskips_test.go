package journal

import (
	"testing"
	"time"
)

func TestCompactTickSkipsDropsOnlyAgedSkipsOverBudget(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	log, _, err := OpenInstanceLog(dir, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	appendEvents := func(events ...Event) {
		t.Helper()
		for _, event := range events {
			if err := log.Append(event); err != nil {
				t.Fatal(err)
			}
		}
	}
	appendEvents(
		Event{Type: EventDaemonStarted},
		Event{Type: EventTickSkipped, Workflow: "w", Reason: "old-1"},
		Event{Type: EventRunnerAnnotation, RunID: "r", Runner: map[string]any{"action": "kept"}},
		Event{Type: EventTickSkipped, Workflow: "w", Reason: "old-2"},
	)
	now = now.Add(48 * time.Hour)
	appendEvents(Event{Type: EventTickSkipped, Workflow: "w", Reason: "recent"})
	cutoff := now.Add(-24 * time.Hour)

	under, err := log.CompactTickSkips(1<<30, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if under.Dropped != 0 || under.BytesRead != 0 {
		t.Fatalf("under-budget pass = %+v, want a stat that reads nothing", under)
	}

	before, err := ReadInstanceLogState(dir)
	if err != nil {
		t.Fatal(err)
	}
	result, err := log.CompactTickSkips(1, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if result.Dropped != 2 || result.Kept != 3 || result.AfterBytes >= result.BeforeBytes {
		t.Fatalf("over-budget pass = %+v, want the two aged skips dropped", result)
	}
	after, err := ReadInstanceLogState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if after.Generation == before.Generation {
		t.Fatal("tick-skip compaction rewrote the open generation instead of advancing it")
	}
	events, err := ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Type != EventDaemonStarted || events[1].Type != EventRunnerAnnotation ||
		events[2].Reason != "recent" || events[2].Seq != 5 {
		t.Fatalf("retained events = %+v, want daemon start, annotation, and the recent skip at its original seq", events)
	}
	appendEvents(Event{Type: EventTickSkipped, Reason: "after"})
	events, err = ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if last := events[len(events)-1]; last.Reason != "after" || last.Seq != 6 {
		t.Fatalf("append after compaction = %+v, want seq 6 on the new generation", last)
	}
}

func TestCompactTickSkipsFencesAGenerationItCannotBringUnderBudget(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	log, _, err := OpenInstanceLog(dir, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	for i := 0; i < 20; i++ {
		if err := log.Append(Event{Type: EventRunnerAnnotation, RunID: "r"}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := log.CompactTickSkips(64, now)
	if err != nil {
		t.Fatal(err)
	}
	if first.Dropped != 0 || first.BytesRead == 0 {
		t.Fatalf("first over-budget pass = %+v, want a full read that drops nothing", first)
	}
	fenced, err := log.CompactTickSkips(64, now)
	if err != nil {
		t.Fatal(err)
	}
	if fenced.BytesRead != 0 {
		t.Fatalf("repeat pass on an unchanged generation read %d bytes, want 0", fenced.BytesRead)
	}
	// A quarter of the budget of growth re-arms the pass.
	if err := log.Append(Event{Type: EventRunnerAnnotation, RunID: "r"}); err != nil {
		t.Fatal(err)
	}
	rearmed, err := log.CompactTickSkips(64, now)
	if err != nil {
		t.Fatal(err)
	}
	if rearmed.BytesRead == 0 {
		t.Fatalf("pass after growth = %+v, want it to read the journal again", rearmed)
	}
}

func TestCompactTickSkipsKeepsTheHighestSequence(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	log, _, err := OpenInstanceLog(dir, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(Event{Type: EventDaemonStarted}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := log.Append(Event{Type: EventTickSkipped, Workflow: "w"}); err != nil {
			t.Fatal(err)
		}
	}
	result, err := log.CompactTickSkips(1, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if result.Dropped != 2 {
		t.Fatalf("pass = %+v, want the two skips below the highest sequence dropped", result)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	// A handle opened later numbers its appends from the generation's tail;
	// had the newest skip been dropped it would reuse sequence 4.
	appendInstanceEvents(t, dir, Event{Type: EventDaemonStarted})
	events, err := ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[1].Seq != 4 || events[2].Seq != 5 {
		t.Fatalf("events after reopen = %+v, want seqs 1, 4, 5", events)
	}
}
