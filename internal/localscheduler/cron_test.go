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

// TestDSTFallBackAmbiguousHour documents (via assertion, not just comment) the
// wrapped schedule's behavior on the repeated wall-clock hour at fall-back: it
// fires once at each UTC offset the ambiguous local time maps to, exactly one
// hour apart, then resumes normal daily cadence. US fall-back 2026 is Nov 1.
func TestDSTFallBackAmbiguousHour(t *testing.T) {
	loc := newYork(t)
	sched, err := ParseSchedule("30 1 * * *")
	if err != nil {
		t.Fatal(err)
	}
	located := InLocation(sched, loc)
	after := time.Date(2026, 10, 31, 12, 0, 0, 0, loc)

	first := located.Next(after)
	second := located.Next(first)
	third := located.Next(second)

	if first.Format("2006-01-02") != "2026-11-01" || second.Format("2006-01-02") != "2026-11-01" {
		t.Fatalf("expected both ambiguous-hour fires on Nov 1: first=%v second=%v", first, second)
	}
	if diff := second.Sub(first); diff != time.Hour {
		t.Fatalf("ambiguous-hour fires should be exactly 1h apart (the repeated hour), got %v", diff)
	}
	if third.Format("2006-01-02") != "2026-11-02" {
		t.Fatalf("expected normal cadence to resume Nov 2, got %v", third)
	}
}
