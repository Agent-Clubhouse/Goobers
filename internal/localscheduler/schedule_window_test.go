package localscheduler

import (
	"testing"
	"time"
)

func TestScheduleWindowBoundsDiscoveryWithoutShiftingIntervalPhase(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, expression string
		elapsed          time.Duration
		count            int
		advance          bool
	}{
		{"minute catch-up", "@every 1m", 3 * time.Hour, 60, true},
		{"long interval not due", "@every 2h", 90 * time.Minute, 0, false},
		{"long interval phase", "@every 2h", 270 * time.Minute, 1, true},
		{"expired interval only", "@every 2h", 210 * time.Minute, 0, true},
		{"expired daily", "0 0 * * *", 84 * time.Hour, 0, true},
		{"dense old interval", "@every 1s", 365 * 24 * time.Hour, 3600, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := ParseSchedule(tc.expression)
			if err != nil {
				t.Fatal(err)
			}
			result, err := EvaluateScheduleWindow([]Schedule{s}, base, base.Add(tc.elapsed))
			if err != nil || result.Tick.MissedTicks != tc.count || result.Tick.Fire != (tc.count > 0) {
				t.Fatal(result, err)
			}
			want := base
			if tc.advance {
				want = base.Add(tc.elapsed)
			}
			if !result.Tick.LastEval.Equal(want) {
				t.Fatal("cursor changed interval phase", result)
			}
		})
	}
}

func TestScheduleWindowSelectsOnlyRecentSources(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	daily, _ := ParseSchedule("0 0 * * *")
	interval, _ := ParseSchedule("@every 30m")
	result, err := EvaluateScheduleWindow([]Schedule{daily, interval}, base, base.Add(84*time.Hour))
	if err != nil || result.Tick.MissedTicks != 2 || len(result.Indexes) != 1 || result.Indexes[0] != 1 {
		t.Fatal(result, err)
	}
}

func TestScheduleWindowPreservesConfiguredDSTBehavior(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	s, err := ParseSchedule("30 1 * * *")
	if err != nil {
		t.Fatal(err)
	}
	s = InLocation(s, loc)
	first := time.Date(2026, 11, 1, 1, 30, 0, 0, loc)
	second := first.Add(time.Hour)
	result, err := EvaluateScheduleWindow([]Schedule{s}, first, second)
	if err != nil || result.Tick.Fire {
		t.Fatal("repeated civil fire replayed", result, err)
	}
}

func TestCapturedScheduleWindowRejectsChangedProvenance(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	schedule, err := ParseSchedule("@every 1m")
	if err != nil {
		t.Fatal(err)
	}
	source := SourceTrigger{ScheduledFrom: base, ScheduledAt: base.Add(3 * time.Hour), ScheduleWindowFrom: base.Add(2 * time.Hour), ScheduleFireCount: 60}
	if _, err := source.scheduleEvaluation([]Schedule{schedule}); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*SourceTrigger){
		func(s *SourceTrigger) { s.ScheduleFireCount-- },
		func(s *SourceTrigger) { s.ScheduleWindowFrom = s.ScheduleWindowFrom.Add(time.Minute) },
	} {
		changed := source
		mutate(&changed)
		if _, err := changed.scheduleEvaluation([]Schedule{schedule}); err == nil {
			t.Fatal("accepted changed schedule provenance", changed)
		}
	}
	changed, err := ParseSchedule("@every 2m")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.scheduleEvaluation([]Schedule{changed}); err == nil {
		t.Fatal("accepted different archived schedule")
	}
}
