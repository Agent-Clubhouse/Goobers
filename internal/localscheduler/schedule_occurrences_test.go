package localscheduler

import (
	"testing"
	"time"
)

func TestOccurrenceProvenanceRejectsChangedDeclarationAndMixedSources(t *testing.T) {
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	schedule, err := ParseSchedule("@every 2m")
	if err != nil {
		t.Fatal(err)
	}
	original := SourceTrigger{ScheduleDeclaration: 1, ScheduledFrom: base, ScheduledAt: base.Add(2 * time.Minute)}
	if err = original.Validate(); err != nil {
		t.Fatal(err)
	}
	result, err := original.scheduleEvaluation([]Schedule{schedule})
	if err != nil || result.Tick.MissedTicks != 1 || len(result.Indexes) != 1 || result.Indexes[0] != 0 {
		t.Fatal(result, err)
	}
	for _, change := range []func(*SourceTrigger){
		func(s *SourceTrigger) { s.ScheduleDeclaration = -1 },
		func(s *SourceTrigger) { s.ScheduleDeclaration = 1001 },
		func(s *SourceTrigger) { s.Signal = "other" },
		func(s *SourceTrigger) { s.WorkerKind = "backlog" },
		func(s *SourceTrigger) { s.ObservedCount = 2 },
		func(s *SourceTrigger) { s.ScheduleFireCount = 1 },
		func(s *SourceTrigger) { s.ScheduleWindowFrom = base },
	} {
		changed := original
		change(&changed)
		if err = changed.Validate(); err == nil {
			t.Fatal("accepted mixed provenance", changed)
		}
	}
	for _, change := range []func(*SourceTrigger){
		func(s *SourceTrigger) { s.ScheduleDeclaration = 2 },
		func(s *SourceTrigger) { s.ScheduledAt = base.Add(time.Minute) },
		func(s *SourceTrigger) { s.ScheduledFrom = base.Add(time.Second) },
	} {
		changed := original
		change(&changed)
		if _, err = changed.scheduleEvaluation([]Schedule{schedule}); err == nil {
			t.Fatal("accepted altered declaration", changed)
		}
	}
}

func TestOccurrenceDiscoveryFollowsTimezoneAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	schedule, err := ParseSchedule("0 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	schedule = locatedSchedule{s: schedule, loc: loc}
	before := time.Date(2026, 3, 7, 2, 0, 0, 0, loc)
	index, at, err := nextScheduleOccurrence([]Schedule{schedule}, []time.Time{before}, time.Date(2026, 3, 10, 0, 0, 0, 0, loc))
	if err != nil || index != 0 || !at.Equal(time.Date(2026, 3, 9, 2, 0, 0, 0, loc)) {
		t.Fatal(index, at, err)
	}
}
