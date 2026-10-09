package startuphint

import (
	"testing"
	"time"
)

func TestBoundFollowsDaemonBudgetAndProgress(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	wait := 20 * time.Minute
	b := NewBound(start, wait, 3*time.Hour)
	at := func(d time.Duration) time.Time { return start.Add(d) }
	expect := func(step string, want time.Time) {
		t.Helper()
		if !b.Deadline().Equal(want) {
			t.Fatalf("%s: deadline = %s after start, want %s", step, b.Deadline().Sub(start), want.Sub(start))
		}
	}

	expect("silent daemon", at(wait))
	b.Observe(at(time.Minute), Hints{})
	expect("no hints", at(wait))

	// A daemon whose own budget outlasts the default is waited for until that
	// budget ends, plus grace.
	b.Observe(at(time.Minute), Hints{HasBudget: true, BudgetRemaining: 40 * time.Minute, Progress: "3"})
	expect("budget", at(time.Minute+40*time.Minute+BudgetGrace))
	// The first token is a baseline; repeating it is not progress.
	b.Observe(at(30*time.Minute), Hints{HasBudget: true, BudgetRemaining: 11 * time.Minute, Progress: "3"})
	expect("same token, shrinking budget", at(46*time.Minute))
	// A spent budget extends nothing on its own.
	b.Observe(at(42*time.Minute), Hints{HasBudget: true, Progress: "3"})
	expect("spent budget", at(46*time.Minute))
	// Progress past the budget earns a fresh stall window from that moment.
	b.Observe(at(45*time.Minute), Hints{HasBudget: true, Progress: "4"})
	expect("progress", at(45*time.Minute+wait))
	// Nothing moves the bound past the cap.
	b.Observe(at(2*time.Hour+50*time.Minute), Hints{Progress: "5"})
	expect("cap", at(3*time.Hour))
}

func TestBoundIgnoresRestartedDaemonsResetProgressAndFreshBudget(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	wait := 20 * time.Minute
	b := NewBound(start, wait, 6*time.Hour)
	at := func(d time.Duration) time.Time { return start.Add(d) }

	b.Observe(at(0), Hints{HasBudget: true, BudgetRemaining: 10 * time.Minute, Progress: "17", Daemon: "a"})
	if !b.Deadline().Equal(at(wait)) {
		t.Fatalf("first daemon: deadline = %s, want %s", b.Deadline().Sub(start), wait)
	}
	// Each restart resets the counter and advertises a fresh full budget;
	// neither may read as the daemon advancing.
	for i, daemon := range []string{"b", "c", "d"} {
		now := at(time.Duration(i+1) * 5 * time.Minute)
		b.Observe(now, Hints{HasBudget: true, BudgetRemaining: time.Hour, Progress: "3", Daemon: daemon})
		if !b.Deadline().Equal(at(wait)) {
			t.Fatalf("restart %s: deadline = %s, want unchanged %s", daemon, b.Deadline().Sub(start), wait)
		}
	}
	// Progress within the restarted process may still extend the bound, but
	// only up to one stall window after the first restart (5m + wait).
	b.Observe(at(18*time.Minute), Hints{HasBudget: true, BudgetRemaining: time.Hour, Progress: "4", Daemon: "d"})
	if want := at(5*time.Minute + wait); !b.Deadline().Equal(want) {
		t.Fatalf("progress after restart: deadline = %s, want %s", b.Deadline().Sub(start), want.Sub(start))
	}
	// A loop whose processes each advance a little cannot chain extensions.
	for i, daemon := range []string{"e", "f", "g"} {
		base := at(19*time.Minute + time.Duration(i)*time.Minute)
		b.Observe(base, Hints{Progress: "1", Daemon: daemon})
		b.Observe(base.Add(30*time.Second), Hints{Progress: "5", Daemon: daemon})
	}
	if want := at(5*time.Minute + wait); !b.Deadline().Equal(want) {
		t.Fatalf("advancing crash loop: deadline = %s, want capped at %s", b.Deadline().Sub(start), want.Sub(start))
	}
}
