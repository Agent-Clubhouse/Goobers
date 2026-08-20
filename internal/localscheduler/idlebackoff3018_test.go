package localscheduler

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

type idleCostStarter struct {
	mu            sync.Mutex
	branchCreates int
	branchDeletes int
}

func (s *idleCostStarter) Start(context.Context, StartRequest) (StartResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.branchCreates++
	s.branchDeletes++
	return StartResult{Phase: journal.PhaseCompleted, NoWork: true}, nil
}

func (s *idleCostStarter) branchOperations() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.branchCreates, s.branchDeletes
}

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

func TestIdleBackoffMateriallyReducesIdleRunAndBranchTelemetry(t *testing.T) {
	const ticks = 60
	runScenario := func(t *testing.T, enabled bool) (int, int, int) {
		t.Helper()
		base := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
		now := base
		starter := &idleCostStarter{}
		scheduler, dir := newTestScheduler(t, []WorkflowEntry{{
			Workflow:  "poll",
			Readiness: apiv1.ReadinessConditions{MaxRunsPerHour: ticks},
			Schedules: []Schedule{fakeSchedule{d: time.Minute}},
			ScheduleBackoffs: []IdleBackoffConfig{{
				Enabled: enabled,
				Floor:   time.Minute,
				Ceiling: 8 * time.Minute,
			}},
			Starter: starter,
		}}, WithClock(func() time.Time { return now }, time.After))

		for minute := 1; minute <= ticks; minute++ {
			now = base.Add(time.Duration(minute) * time.Minute)
			scheduler.Tick(context.Background(), now)
			scheduler.Wait()
		}

		events, err := journal.ReadInstanceLog(dir)
		if err != nil {
			t.Fatal(err)
		}
		noWorkRuns := 0
		for _, event := range events {
			if event.Type == journal.EventRunStarted {
				noWorkRuns++
			}
		}
		creates, deletes := starter.branchOperations()
		return noWorkRuns, creates, deletes
	}

	withoutBackoffRuns, withoutBackoffCreates, withoutBackoffDeletes := runScenario(t, false)
	withBackoffRuns, withBackoffCreates, withBackoffDeletes := runScenario(t, true)
	if withoutBackoffRuns != ticks || withoutBackoffCreates != ticks || withoutBackoffDeletes != ticks {
		t.Fatalf("fixed-cadence telemetry = runs:%d creates:%d deletes:%d, want %d each",
			withoutBackoffRuns, withoutBackoffCreates, withoutBackoffDeletes, ticks)
	}
	if withBackoffRuns*2 >= withoutBackoffRuns ||
		withBackoffCreates*2 >= withoutBackoffCreates ||
		withBackoffDeletes*2 >= withoutBackoffDeletes {
		t.Fatalf("backoff did not cut idle costs by more than half: enabled runs:%d creates:%d deletes:%d; disabled runs:%d creates:%d deletes:%d",
			withBackoffRuns, withBackoffCreates, withBackoffDeletes,
			withoutBackoffRuns, withoutBackoffCreates, withoutBackoffDeletes)
	}
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

func TestIdleBackoffDoesNotSuppressOrMutateBacklogTrigger(t *testing.T) {
	base := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	now := base
	counter := &fakeBacklogCounter{}
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted, NoWork: true}}
	scheduler, _ := newTestScheduler(t, []WorkflowEntry{{
		Workflow:       "mixed",
		Schedules:      []Schedule{fakeSchedule{d: time.Minute}},
		BacklogCounter: counter,
		ScheduleBackoffs: []IdleBackoffConfig{{
			Enabled: true,
			Floor:   time.Minute,
			Ceiling: 4 * time.Minute,
		}},
		Starter: starter,
	}}, WithClock(func() time.Time { return now }, time.After))

	for _, minute := range []int{1, 2} {
		now = base.Add(time.Duration(minute) * time.Minute)
		scheduler.Tick(context.Background(), now)
		scheduler.Wait()
	}
	counter.mu.Lock()
	counter.count = 1
	counter.mu.Unlock()
	now = base.Add(3 * time.Minute)
	scheduler.Tick(context.Background(), now)
	scheduler.Wait()

	counter.mu.Lock()
	counter.count = 0
	counter.mu.Unlock()
	now = base.Add(4 * time.Minute)
	scheduler.Tick(context.Background(), now)
	scheduler.Wait()

	starter.mu.Lock()
	defer starter.mu.Unlock()
	if len(starter.starts) != 4 {
		t.Fatalf("starts = %d, want two initial schedule runs, one backlog run during backoff, and the next due schedule run", len(starter.starts))
	}
	want := []journal.TriggerKind{
		journal.TriggerSchedule,
		journal.TriggerSchedule,
		journal.TriggerItem,
		journal.TriggerSchedule,
	}
	for index, request := range starter.starts {
		if request.Trigger.Kind != want[index] {
			t.Fatalf("start %d trigger = %q, want %q", index, request.Trigger.Kind, want[index])
		}
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
