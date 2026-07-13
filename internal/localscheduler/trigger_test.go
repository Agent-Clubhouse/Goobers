package localscheduler

import (
	"testing"
	"time"
)

// fakeSchedule fires every d, starting at the first multiple of d after "after".
type fakeSchedule struct{ d time.Duration }

func (f fakeSchedule) Next(after time.Time) time.Time {
	return after.Add(f.d)
}

func TestTickFiresWhenDue(t *testing.T) {
	sched := fakeSchedule{d: time.Hour}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := TriggerState{Workflow: "wf", Schedule: sched, LastEval: base}

	res := Tick(ts, base.Add(30*time.Minute))
	if res.Fire {
		t.Fatalf("should not be due yet: %+v", res)
	}
	if res.LastEval != base {
		t.Errorf("LastEval should be unchanged when not due: %v", res.LastEval)
	}

	res = Tick(ts, base.Add(time.Hour))
	if !res.Fire || res.CatchUp {
		t.Fatalf("expected an on-time fire, not catch-up: %+v", res)
	}
	if res.LastEval != base.Add(time.Hour) {
		t.Errorf("LastEval should advance to now on fire: %v", res.LastEval)
	}
}

// TestTickCollapsesMissedTicksToOne is the missed-tick policy under test: a
// daemon outage spanning several scheduled fires produces exactly one catch-up
// fire, and LastEval advances to now (not to the next unfired tick) so no
// backlog replays on the following tick.
func TestTickCollapsesMissedTicksToOne(t *testing.T) {
	sched := fakeSchedule{d: time.Hour}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := TriggerState{Workflow: "wf", Schedule: sched, LastEval: base}

	// Daemon was down for 5 hours' worth of ticks.
	now := base.Add(5 * time.Hour)
	res := Tick(ts, now)
	if !res.Fire || !res.CatchUp {
		t.Fatalf("expected a catch-up fire: %+v", res)
	}
	if res.MissedTicks != 5 {
		t.Errorf("MissedTicks = %d, want 5", res.MissedTicks)
	}
	if res.LastEval != now {
		t.Fatalf("LastEval should collapse to now, got %v want %v", res.LastEval, now)
	}

	// The very next tick (immediately after) must NOT fire again — no backlog.
	ts.LastEval = res.LastEval
	res2 := Tick(ts, now.Add(time.Minute))
	if res2.Fire {
		t.Fatalf("missed-tick collapse should not leave a backlog to replay: %+v", res2)
	}
}

func TestTickHandlesExactlyOneMissedTick(t *testing.T) {
	sched := fakeSchedule{d: time.Hour}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := TriggerState{Workflow: "wf", Schedule: sched, LastEval: base}

	res := Tick(ts, base.Add(time.Hour+time.Second))
	if !res.Fire || res.CatchUp {
		t.Fatalf("a single due tick evaluated slightly late is on-time, not catch-up: %+v", res)
	}
}

func TestReconstructLastEval(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seen := start.Add(2 * time.Hour)
	fired := []TriggerFiredRecord{
		{Workflow: "a", Time: start.Add(1 * time.Hour)},
		{Workflow: "a", Time: seen}, // most recent for "a"
		{Workflow: "b", Time: start.Add(30 * time.Minute)},
	}
	last := ReconstructLastEval(fired, []string{"a", "b", "c"}, start)

	if last["a"] != seen {
		t.Errorf("workflow a: LastEval = %v, want most recent fire %v", last["a"], seen)
	}
	if last["b"] != start.Add(30*time.Minute) {
		t.Errorf("workflow b: LastEval = %v", last["b"])
	}
	if last["c"] != start {
		t.Errorf("workflow c (never fired): LastEval = %v, want daemon start %v (no epoch backfill)", last["c"], start)
	}
}
