package startintent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type scheduleStarterFunc func(context.Context, localscheduler.StartRequest) (localscheduler.StartResult, error)

func (f scheduleStarterFunc) Start(ctx context.Context, req localscheduler.StartRequest) (localscheduler.StartResult, error) {
	return f(ctx, req)
}

func scheduleDispatchFixture(t *testing.T, starter localscheduler.Starter, now *time.Time) (*Service, *localscheduler.Scheduler, *Sources, localscheduler.WorkflowEntry) {
	t.Helper()
	source, entry := sourceFixture(t)
	schedule, err := localscheduler.ParseSchedule("@every 1m")
	if err != nil {
		t.Fatal(err)
	}
	entry.Schedules = []localscheduler.Schedule{schedule}
	entry.ScheduleBackoffs = []localscheduler.IdleBackoffConfig{{Enabled: true, Floor: time.Minute, Ceiling: 4 * time.Minute}}
	entry.Starter = starter
	log, _, err := journal.OpenInstanceLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	scheduler := localscheduler.New([]localscheduler.WorkflowEntry{entry}, log, localscheduler.WithSourceQueue(source), localscheduler.WithClock(func() time.Time { return *now }, time.After))
	if err := scheduler.ReconcileAll(nil, *now); err != nil {
		t.Fatal(err)
	}
	service := &Service{Queue: source.Queue, Now: func() time.Time { return *now }, Scheduler: func() *localscheduler.Scheduler { return scheduler }, RunDirectory: func(context.Context, string) (string, error) { return "", nil }, Build: func(context.Context, Target) (Prepared, error) {
		return Prepared{Entry: entry, Release: func() {}}, nil
	}}
	return service, scheduler, source, entry
}

func TestSourceScheduleCapacityPreservesAcceptedFiring(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan localscheduler.StartRequest, 2)
	release := make(chan struct{})
	starter := scheduleStarterFunc(func(ctx context.Context, req localscheduler.StartRequest) (localscheduler.StartResult, error) {
		started <- req
		select {
		case <-release:
		case <-ctx.Done():
			return localscheduler.StartResult{}, ctx.Err()
		}
		return localscheduler.StartResult{Phase: journal.PhaseCompleted}, nil
	})
	now := time.Now().UTC().Truncate(time.Minute)
	service, scheduler, _, _ := scheduleDispatchFixture(t, starter, &now)
	t.Cleanup(func() { cancel(); scheduler.Wait() })
	firstDue := now.Add(time.Minute)
	scheduler.Tick(ctx, firstDue)
	pending, err := service.Queue.Pending(ctx, 10)
	if err != nil || len(pending) != 1 {
		t.Fatal(pending, err)
	}
	first := pending[0]
	if err := service.Dispatch(ctx, ctx, first); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	scheduler.Tick(ctx, firstDue.Add(time.Minute))
	pending, err = service.Queue.Pending(ctx, 10)
	if err != nil || len(pending) != 1 {
		t.Fatal(pending, err)
	}
	second := pending[0]
	if err := service.Dispatch(ctx, ctx, second); err != nil {
		t.Fatal(err)
	}
	held, err := service.Queue.Get(ctx, second.ID, "scheduler")
	if err != nil || held.State != triggerqueue.Accepted || held.RunID != "" {
		t.Fatal(held, err)
	}
	select {
	case <-started:
		t.Fatal("launched through occupied capacity")
	default:
	}
	close(release)
	scheduler.Wait()
	if err := service.Dispatch(ctx, ctx, held); err != nil {
		t.Fatal(err)
	}
	scheduler.Wait()
	select {
	case req := <-started:
		if req.RunID != strings.TrimPrefix(second.ID, "trigger-") {
			t.Fatal("changed queued run identity", req.RunID)
		}
	default:
		t.Fatal("accepted firing did not start after capacity released")
	}
}

func TestSourceScheduleBackoffCommitsSkipWithoutLosingCursor(t *testing.T) {
	starts := 0
	starter := scheduleStarterFunc(func(context.Context, localscheduler.StartRequest) (localscheduler.StartResult, error) {
		starts++
		return localscheduler.StartResult{Phase: journal.PhaseCompleted, NoWork: true}, nil
	})
	base := time.Now().UTC().Truncate(time.Minute)
	now := base
	service, scheduler, source, entry := scheduleDispatchFixture(t, starter, &now)
	for minute := 1; minute <= 4; minute++ {
		now = base.Add(time.Duration(minute) * time.Minute)
		scheduler.Tick(t.Context(), now)
		pending, err := service.Queue.Pending(t.Context(), 10)
		if err != nil {
			t.Fatal(err)
		}
		if minute == 3 && len(pending) != 0 {
			t.Fatal("idle backoff enqueued another poll", pending)
		}
		for _, record := range pending {
			if err := service.Dispatch(t.Context(), t.Context(), record); err != nil {
				t.Fatal(err)
			}
			scheduler.Wait()
		}
		cursor, _, err := source.ScheduleCursor(t.Context(), entry, base, false)
		if err != nil || !cursor.Equal(now) {
			t.Fatal(cursor, err)
		}
	}
	if starts != 3 {
		t.Fatalf("got %d starts; want 3 with minute 3 suppressed", starts)
	}
}
