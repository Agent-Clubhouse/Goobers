package localscheduler

import (
	"testing"
	"time"
)

func newYork(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	return loc
}

func TestParseScheduleAccepts(t *testing.T) {
	for _, expr := range []string{
		"*/5 * * * *", "0 9 * * 1-5", "@hourly", "@daily", "@every 90m",
	} {
		if _, err := ParseSchedule(expr); err != nil {
			t.Errorf("ParseSchedule(%q): %v", expr, err)
		}
	}
}

func TestParseScheduleRejects6Field(t *testing.T) {
	_, err := ParseSchedule("0 30 2 * * *")
	if err == nil {
		t.Fatal("expected 6-field cron to be rejected in V0")
	}
}

func TestParseScheduleRejectsGarbage(t *testing.T) {
	if _, err := ParseSchedule("not a cron"); err == nil {
		t.Fatal("expected an error for a malformed expression")
	}
}

func TestNextScheduledFire(t *testing.T) {
	hourly, err := ParseSchedule("@hourly")
	if err != nil {
		t.Fatal(err)
	}
	daily, err := ParseSchedule("@daily")
	if err != nil {
		t.Fatal(err)
	}
	after := time.Date(2026, time.July, 20, 6, 30, 0, 0, time.UTC)

	got, ok := NextScheduledFire([]Schedule{daily, hourly}, after)
	if !ok || !got.Equal(time.Date(2026, time.July, 20, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("NextScheduledFire() = %v, %t", got, ok)
	}
	if got, ok := NextScheduledFire(nil, after); ok || !got.IsZero() {
		t.Fatalf("NextScheduledFire(nil) = %v, %t, want zero, false", got, ok)
	}
}

// TestDSTSpringForwardSkipsNonexistentTime verifies the wrapped cron.Schedule
// skips the day a wall-clock time doesn't exist rather than erroring or
// producing an invalid instant. US spring-forward 2026 is March 8: 2:00am
// jumps to 3:00am, so 2:30am does not occur that day.
func TestDSTSpringForwardSkipsNonexistentTime(t *testing.T) {
	loc := newYork(t)
	sched, err := ParseSchedule("30 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	located := InLocation(sched, loc)
	after := time.Date(2026, 3, 7, 12, 0, 0, 0, loc)

	next := located.Next(after)
	if got := next.Format("2006-01-02"); got == "2026-03-08" {
		t.Fatalf("fired on the transition day where 02:30 does not exist: %v", next)
	}
	if got, want := next.Format("2006-01-02"), "2026-03-09"; got != want {
		t.Fatalf("next fire = %s, want %s (skip the nonexistent day)", got, want)
	}
}

// TestDSTDailyFiringStaysWallClockStable verifies a schedule at a time that
// exists every day (10am) keeps firing at the same wall-clock hour across the
// spring-forward boundary — calendar-based, not a fixed 24h duration.
func TestDSTDailyFiringStaysWallClockStable(t *testing.T) {
	loc := newYork(t)
	sched, err := ParseSchedule("0 10 * * *")
	if err != nil {
		t.Fatal(err)
	}
	located := InLocation(sched, loc)
	after := time.Date(2026, 3, 7, 12, 0, 0, 0, loc)

	for i, wantDate := range []string{"2026-03-08", "2026-03-09", "2026-03-10", "2026-03-11"} {
		next := located.Next(after)
		if got := next.Format("2006-01-02"); got != wantDate {
			t.Fatalf("fire %d: date=%s want %s", i, got, wantDate)
		}
		if next.Hour() != 10 || next.Minute() != 0 {
			t.Fatalf("fire %d: wall clock drifted to %v", i, next)
		}
		after = next
	}
}

// TestDSTFallBackFiresOnceNotTwice is issue #137's fall-back fix: the
// underlying robfig/cron schedule's Next has no notion of "this civil slot
// already fired" — on the repeated wall-clock hour at fall-back, calling
// Next(after the first fire) legitimately returns the SAME wall-clock
// reading's second real occurrence (one real hour later, a different
// absolute instant) as "strictly after" the first. Before this fix that
// meant a daily schedule fired twice, once at each UTC offset the ambiguous
// local time maps to — a guaranteed once-a-year double-fire for a scheduler
// whose runs cost money and mutate GitHub state. InLocation's Next now
// detects that same-civil-minute repeat and skips past it, so the schedule
// fires exactly once for the nominal slot and resumes normal daily cadence
// immediately after. US fall-back 2026 is Nov 1.
func TestDSTFallBackFiresOnceNotTwice(t *testing.T) {
	loc := newYork(t)
	sched, err := ParseSchedule("30 1 * * *")
	if err != nil {
		t.Fatal(err)
	}
	located := InLocation(sched, loc)
	after := time.Date(2026, 10, 31, 12, 0, 0, 0, loc)

	first := located.Next(after)
	if first.Format("2006-01-02 15:04") != "2026-11-01 01:30" {
		t.Fatalf("expected the one legitimate fire on Nov 1 at 01:30, got %v", first)
	}

	// The very next call — simulating the scheduler's subsequent tick, using
	// this fire as the new baseline — must NOT return the repeated-hour's
	// second occurrence (Nov 1 01:30 EST, one real hour later): it must skip
	// straight to normal next-day cadence.
	second := located.Next(first)
	if second.Format("2006-01-02 15:04") != "2026-11-02 01:30" {
		t.Fatalf("expected normal cadence to resume Nov 2 01:30 (not a repeat of Nov 1's ambiguous hour), got %v", second)
	}
	if diff := second.Sub(first); diff < 20*time.Hour {
		t.Fatalf("expected roughly a calendar day between fires (the fall-back day is 25 real hours), got a suspiciously short gap: %v", diff)
	}
}

// TestDSTFallBackDoesNotAffectUnrelatedHours is the negative control for the
// fall-back fix: fires whose wall-clock reading genuinely differs between
// calls (not a repeat of the same civil minute) must advance normally, even
// immediately adjacent to a DST transition — the fix must not over-suppress
// anything beyond the exact repeated-slot pattern.
//
// Known, accepted simplification: an hourly (or any sub-daily wildcard-field)
// schedule's OWN repeated hour at fall-back also collapses to one fire here,
// same as a single-valued daily schedule — InLocation's Next has no
// visibility into which of the original cron expression's fields were fixed
// values versus wildcards/ranges (robfig/cron's Schedule interface exposes
// only Next(time.Time) time.Time, not the parsed field spec), so it can't
// distinguish "a single-valued 30 1 * * * that should fire once" from "a
// 0 * * * * that arguably should fire once per real hour, including the
// repeated one." Collapsing both is the safer default for V0's actual
// schedules (workflow triggers, invariably daily-or-longer per
// ARCHITECTURE.md §7) and strictly no worse than the pre-fix behavior, which
// double-fired unconditionally; a follow-up could track field-value-vs-range
// per entry if a genuine hourly-heartbeat use case needs the other choice.
func TestDSTFallBackDoesNotAffectUnrelatedHours(t *testing.T) {
	loc := newYork(t)
	sched, err := ParseSchedule("0 * * * *") // every hour on the hour
	if err != nil {
		t.Fatal(err)
	}
	located := InLocation(sched, loc)
	after := time.Date(2026, 10, 31, 22, 30, 0, 0, loc)

	var got []string
	next := after
	for i := 0; i < 3; i++ {
		next = located.Next(next)
		got = append(got, next.Format("2006-01-02 15:04 -0700"))
	}
	want := []string{
		"2026-10-31 23:00 -0400", // well before the transition: unaffected
		"2026-11-01 00:00 -0400", // last EDT hour before fall-back: unaffected
		"2026-11-01 01:00 -0400", // the repeated hour's first occurrence only
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("fire %d = %s, want %s (full sequence: %v)", i, got[i], want[i], got)
		}
	}
}
