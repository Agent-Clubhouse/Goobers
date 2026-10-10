package startintent

import (
	"testing"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
)

func TestScheduleWindowExpiredDemandDoesNotPollOrResurrect(t *testing.T) {
	source, entry, counter, base := demandFixture(t)
	s, err := localscheduler.ParseSchedule("@every 2h")
	if err != nil {
		t.Fatal(err)
	}
	entry.Schedules = []localscheduler.Schedule{s}
	scheduler := workerScheduler(t, source, entry)
	if err := scheduler.ReconcileAll(nil, base); err != nil {
		t.Fatal(err)
	}
	now := base.Add(210 * time.Minute) // The 2h fire is outside the preceding hour.
	scheduler.Tick(t.Context(), now)
	workerPending(t, source, 0)
	if counter.calls != 0 {
		t.Fatal("expired demand polled", counter.calls)
	}
	cursor, _, err := source.ScheduleCursor(t.Context(), entry, base, false, now)
	if err != nil || !cursor.Equal(now) {
		t.Fatal(cursor, err)
	}
	restarted := workerScheduler(t, source, entry)
	if err := restarted.ReconcileAll(nil, base); err != nil {
		t.Fatal(err)
	}
	restarted.Tick(t.Context(), now)
	workerPending(t, source, 0)
	if counter.calls != 0 {
		t.Fatal("expired firing resurrected", counter.calls)
	}
}

func TestScheduleWindowDoesNotExpireAcceptedDemand(t *testing.T) {
	source, entry, counter, base := demandFixture(t)
	entry.Readiness.MaxConcurrentRuns = 1
	counter.count = 2
	scheduler := workerScheduler(t, source, entry)
	if err := scheduler.ReconcileAll(nil, base); err != nil {
		t.Fatal(err)
	}
	scheduler.Tick(t.Context(), base.Add(time.Hour))
	first := workerPending(t, source, 1)
	finishDemandStarts(t, source.Queue, first)
	// Existing accepted custody survives even though later discovery is bounded.
	scheduler.Tick(t.Context(), base.Add(12*time.Hour))
	second := workerPending(t, source, 1)
	envelope, err := Parse(second[0].Payload)
	if err != nil || !envelope.Source.ScheduledAt.Equal(base.Add(time.Hour)) || envelope.Source.ScheduleOrdinal != 2 || counter.calls != 1 {
		t.Fatal(envelope, counter.calls, err)
	}
}
