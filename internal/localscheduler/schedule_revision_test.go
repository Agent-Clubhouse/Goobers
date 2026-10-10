package localscheduler

import (
	"testing"
	"time"
)

func TestScheduleRevisionUsesAuthoredScheduleAndTimezone(t *testing.T) {
	parse := func(expr, zone string) Schedule {
		t.Helper()
		s, err := ParseSchedule(expr)
		if err != nil {
			t.Fatal(err)
		}
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatal(err)
		}
		return InLocation(s, loc)
	}
	revision := func(s Schedule) string {
		t.Helper()
		rev, err := ScheduleRevision([]Schedule{s})
		if err != nil {
			t.Fatal(err)
		}
		return rev
	}
	original := revision(parse("0  9 * * *", "UTC"))
	if original != revision(parse(" 0\t9 * * * ", "UTC")) {
		t.Fatal("format-only edit reset schedule")
	}
	if original == revision(parse("0 10 * * *", "UTC")) {
		t.Fatal("changed schedule kept revision")
	}
	if original == revision(parse("0 9 * * *", "America/Chicago")) {
		t.Fatal("changed timezone kept revision")
	}
}
