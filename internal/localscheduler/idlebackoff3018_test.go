package localscheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestIdleBackoffSuppressesConsecutiveNoWorkPolls(t *testing.T) {
	base := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	now := base
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted, NoWork: true}}
	scheduler, dir := newTestScheduler(t, []WorkflowEntry{{
		Workflow:  "poll",
		Schedules: []Schedule{fakeSchedule{d: time.Minute}},
		ScheduleBackoffs: []IdleBackoffConfig{{
			Enabled: true,
			Floor:   time.Minute,
			Ceiling: 4 * time.Minute,
		}},
		Starter: starter,
	}}, WithClock(func() time.Time { return now }, time.After))

	for _, tickAt := range []time.Time{
		base.Add(time.Minute),
		base.Add(2 * time.Minute),
		base.Add(3 * time.Minute),
		base.Add(4 * time.Minute),
	} {
		now = tickAt
		scheduler.Tick(context.Background(), tickAt)
		scheduler.Wait()
	}

	if got := starter.count(); got != 3 {
		t.Fatalf("starts = %d, want 3: the third configured tick should be suppressed", got)
	}
	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == journal.EventTickSkipped && strings.HasPrefix(event.Reason, "idle backoff:") {
			return
		}
	}
	t.Fatal("idle backoff suppression was not journaled")
}

func TestIdleBackoffDoesNotDelaySustainedWork(t *testing.T) {
	base := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	now := base
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	scheduler, _ := newTestScheduler(t, []WorkflowEntry{{
		Workflow:  "poll",
		Schedules: []Schedule{fakeSchedule{d: time.Minute}},
		Starter:   starter,
	}}, WithClock(func() time.Time { return now }, time.After))

	for minute := 1; minute <= 4; minute++ {
		now = base.Add(time.Duration(minute) * time.Minute)
		scheduler.Tick(context.Background(), now)
		scheduler.Wait()
	}

	if got := starter.count(); got != 4 {
		t.Fatalf("starts = %d, want every one of 4 configured ticks under sustained work", got)
	}
}

func TestSignalResetsIdleBackoffImmediately(t *testing.T) {
	base := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	now := base
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted, NoWork: true}}
	scheduler, _ := newTestScheduler(t, []WorkflowEntry{{
		Workflow:  "poll",
		Schedules: []Schedule{fakeSchedule{d: time.Minute}},
		Signals:   []string{"work-ready"},
		ScheduleBackoffs: []IdleBackoffConfig{{
			Enabled: true,
			Floor:   time.Minute,
			Ceiling: 4 * time.Minute,
		}},
		Starter: starter,
	}}, WithClock(func() time.Time { return now }, time.After))

	for _, minute := range []int{1, 2, 4} {
		now = base.Add(time.Duration(minute) * time.Minute)
		scheduler.Tick(context.Background(), now)
		scheduler.Wait()
	}

	now = base.Add(5 * time.Minute)
	runIDs := scheduler.Signal(context.Background(), "work-ready", now)
	if len(runIDs) != 1 {
		t.Fatalf("signal run IDs = %v, want one", runIDs)
	}
	scheduler.Wait()
	now = base.Add(6 * time.Minute)
	scheduler.Tick(context.Background(), now)
	scheduler.Wait()

	if got := starter.count(); got != 5 {
		t.Fatalf("starts = %d, want three polls, one signal run, and the next cadence poll", got)
	}
}

func TestIdleBackoffCanBeDisabled(t *testing.T) {
	base := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	now := base
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted, NoWork: true}}
	scheduler, _ := newTestScheduler(t, []WorkflowEntry{{
		Workflow:         "poll",
		Schedules:        []Schedule{fakeSchedule{d: time.Minute}},
		ScheduleBackoffs: []IdleBackoffConfig{{Enabled: false}},
		Starter:          starter,
	}}, WithClock(func() time.Time { return now }, time.After))

	for minute := 1; minute <= 4; minute++ {
		now = base.Add(time.Duration(minute) * time.Minute)
		scheduler.Tick(context.Background(), now)
		scheduler.Wait()
	}

	if got := starter.count(); got != 4 {
		t.Fatalf("starts = %d, want fixed cadence when backoff is disabled", got)
	}
}

func TestParseIdleBackoffDefaultsAndBounds(t *testing.T) {
	config, err := ParseIdleBackoff(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !config.Enabled || config.Floor != time.Minute || config.Ceiling != 15*time.Minute {
		t.Fatalf("default config = %+v", config)
	}

	disabled := false
	config, err = ParseIdleBackoff(&apiv1.IdleBackoff{
		Enabled: &disabled,
		Floor:   "2m",
		Ceiling: "10m",
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.Enabled || config.Floor != 2*time.Minute || config.Ceiling != 10*time.Minute {
		t.Fatalf("configured backoff = %+v", config)
	}
	if _, err := ParseIdleBackoff(&apiv1.IdleBackoff{Floor: "10m", Ceiling: "2m"}); err == nil {
		t.Fatal("ceiling below floor succeeded")
	}
}
