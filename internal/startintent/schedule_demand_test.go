package startintent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type demandTestCounter struct {
	count, calls int
	err          error
}

func (c *demandTestCounter) EligibleCount(context.Context) (int, error) {
	c.calls++
	return c.count, c.err
}
func demandFixture(t *testing.T) (*Sources, localscheduler.WorkflowEntry, *demandTestCounter, time.Time) {
	t.Helper()
	source, entry := sourceFixture(t)
	schedule, err := localscheduler.ParseSchedule("@every 1h")
	if err != nil {
		t.Fatal(err)
	}
	counter := &demandTestCounter{count: 5}
	entry.ScheduleDemandCounter = counter
	entry.Schedules = []localscheduler.Schedule{schedule}
	entry.Readiness.MaxConcurrentRuns = 2
	entry.Starter = &workerStarter{}
	source.Build = func(_ context.Context, target Target) (Prepared, error) {
		if target.ConfigGeneration != entry.ConfigGeneration {
			t.Fatal(target)
		}
		return Prepared{Entry: entry, Release: func() {}}, nil
	}
	return source, entry, counter, time.Now().UTC().Truncate(time.Hour)
}
func finishDemandStarts(t *testing.T, q *triggerqueue.Store, records []triggerqueue.Record) {
	t.Helper()
	for _, r := range records {
		if err := q.BeginDispatch(t.Context(), r.ID); err != nil {
			t.Fatal(err)
		}
		if err := q.Finish(t.Context(), r.ID, triggerqueue.Rejected, "", "test capacity released", r.AcceptedAt.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDemandScheduleSealsCountAndTransfersOriginalPinsAcrossRestart(t *testing.T) {
	source, entry, counter, base := demandFixture(t)
	path := filepath.Join(t.TempDir(), "queue.db")
	queue, err := triggerqueue.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	source.Queue = queue
	t.Cleanup(func() { _ = source.Queue.Close() })
	scheduler := workerScheduler(t, source, entry)
	if err = scheduler.ReconcileAll(nil, base); err != nil {
		t.Fatal(err)
	}
	scheduler.Tick(t.Context(), base.Add(time.Hour))
	first := workerPending(t, source, 2)
	d, _, err := source.LoadDemand(t.Context(), entry)
	if err != nil || d.Count != 5 || d.Queued != 2 {
		t.Fatal(d, err)
	}
	if counter.calls != 1 {
		t.Fatal(counter.calls)
	}
	pins, err := RetainedGenerations(t.Context(), source.Queue)
	if err != nil || !pins[entry.ConfigGeneration] {
		t.Fatal(pins, err)
	}
	// Crash after observation/transfer: reopen the same ledger and use a newer
	// applied generation. The original count and definition remain authoritative.
	if err = queue.Close(); err != nil {
		t.Fatal(err)
	}
	source.Queue, err = triggerqueue.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	source.Build = func(context.Context, Target) (Prepared, error) {
		t.Fatal("sealed count was re-polled/rebuilt")
		return Prepared{}, nil
	}
	newer := entry
	newer.ConfigGeneration = "new-generation"
	newer.ScheduleDemandCounter = &demandTestCounter{count: 1000}
	changed, err := localscheduler.ParseSchedule("@every 2h")
	if err != nil {
		t.Fatal(err)
	}
	newer.Schedules = []localscheduler.Schedule{changed}
	restarted := workerScheduler(t, source, newer)
	if err = restarted.ReconcileAll(nil, base.Add(20*time.Hour)); err != nil {
		t.Fatal(err)
	}
	restarted.Tick(t.Context(), base.Add(time.Hour))
	workerPending(t, source, 2)
	all := append([]triggerqueue.Record(nil), first...)
	finishDemandStarts(t, source.Queue, first)
	restarted.Tick(t.Context(), base.Add(time.Hour+time.Second))
	second := workerPending(t, source, 2)
	all = append(all, second...)
	finishDemandStarts(t, source.Queue, second)
	restarted.Tick(t.Context(), base.Add(time.Hour+2*time.Second))
	third := workerPending(t, source, 1)
	all = append(all, third...)
	ordinals := map[int]bool{}
	for _, r := range all {
		e, err := Parse(r.Payload)
		if err != nil || e.Target.ConfigGeneration != entry.ConfigGeneration || !e.Source.ScheduledAt.Equal(base.Add(time.Hour)) {
			t.Fatal(e, err)
		}
		if e.Source.ScheduleCount != 5 || ordinals[e.Source.ScheduleOrdinal] {
			t.Fatal("duplicate/missing scheduled ordinal", e.Source)
		}
		ordinals[e.Source.ScheduleOrdinal] = true
	}
	if d, _, err = source.LoadDemand(t.Context(), newer); err != nil || d != nil {
		t.Fatal(d, err)
	}
	if entry.Starter.(*workerStarter).calls.Load() != 0 {
		t.Fatal("sizing bypassed queued execution")
	}
}

func TestDemandScheduleRetainsUnobservedFailureAndUsesPinnedCounterAfterReload(t *testing.T) {
	source, entry, counter, base := demandFixture(t)
	counter.err = errors.New("temporarily unknown count")
	scheduler := workerScheduler(t, source, entry)
	if err := scheduler.ReconcileAll(nil, base); err != nil {
		t.Fatal(err)
	}
	scheduler.Tick(t.Context(), base.Add(time.Hour))
	workerPending(t, source, 0)
	d, release, err := source.LoadDemand(t.Context(), entry)
	if release != nil {
		release()
	}
	if err != nil || d == nil || d.Count != -1 {
		t.Fatal(d, err)
	}
	counter.err = nil
	counter.count = 1
	newer := entry
	newer.ConfigGeneration = "new"
	newer.ScheduleDemandCounter = &demandTestCounter{count: 50}
	restarted := workerScheduler(t, source, newer)
	if err = restarted.ReconcileAll(nil, base.Add(10*time.Hour)); err != nil {
		t.Fatal(err)
	}
	restarted.Tick(t.Context(), base.Add(time.Hour+time.Second))
	records := workerPending(t, source, 1)
	e, err := Parse(records[0].Payload)
	if err != nil || e.Target.ConfigGeneration != entry.ConfigGeneration || counter.calls != 2 {
		t.Fatal(e, counter.calls, err)
	}
}

func TestDemandScheduleCaptureFailureNeverFallsBackToDirectStart(t *testing.T) {
	source, entry, _, base := demandFixture(t)
	source.Acquire = func(context.Context, Target) (func(), error) { return nil, errors.New("archive unavailable") }
	scheduler := workerScheduler(t, source, entry)
	if err := scheduler.ReconcileAll(nil, base); err != nil {
		t.Fatal(err)
	}
	scheduler.Tick(t.Context(), base.Add(time.Hour))
	workerPending(t, source, 0)
	if entry.Starter.(*workerStarter).calls.Load() != 0 {
		t.Fatal("missing archive bypassed durable custody")
	}
	cursor, _, err := source.ScheduleCursor(t.Context(), entry, base.Add(10*time.Hour), false, base.Add(10*time.Hour))
	if err != nil || !cursor.Equal(base) {
		t.Fatal(cursor, err)
	}
}

func TestDemandScheduleNoWorkAndFallbackAreDurable(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-work", true: "timeout-one-worker"}[fallback], func(t *testing.T) {
			source, entry, counter, base := demandFixture(t)
			counter.count = 0
			if fallback {
				counter.err = context.DeadlineExceeded
			}
			scheduler := workerScheduler(t, source, entry)
			if err := scheduler.ReconcileAll(nil, base); err != nil {
				t.Fatal(err)
			}
			scheduler.Tick(t.Context(), base.Add(time.Hour))
			want := 0
			if fallback {
				want = 1
			}
			workerPending(t, source, want)
			scheduler.Tick(t.Context(), base.Add(time.Hour+time.Second))
			workerPending(t, source, want)
			if counter.calls != 1 {
				t.Fatal("completed fire repolled", counter.calls)
			}
		})
	}
}

func TestDemandScheduleReleasedCapacityWakesRemainingWorkers(t *testing.T) {
	source, entry, counter, base := demandFixture(t)
	entry.Readiness.MaxConcurrentRuns = 1
	counter.count = 2
	now := base.Add(time.Hour)
	log, _, err := journal.OpenInstanceLog(filepath.Join(t.TempDir(), "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	ticks := make(chan struct{}, 4)
	never := make(chan time.Time)
	scheduler := localscheduler.New([]localscheduler.WorkflowEntry{entry}, log, localscheduler.WithSourceQueue(source), localscheduler.WithClock(func() time.Time { return now }, func(time.Duration) <-chan time.Time { return never }), localscheduler.WithAfterTick(func(context.Context) { ticks <- struct{}{} }))
	if err = scheduler.ReconcileAll(nil, base); err != nil {
		t.Fatal(err)
	}
	scheduler.Tick(t.Context(), now)
	<-ticks
	first := workerPending(t, source, 1)[0]
	runID := strings.TrimPrefix(first.ID, "trigger-")
	release, ok, reason := scheduler.ReserveContinuation(runID, entry.Gaggle, entry.Workflow)
	if !ok {
		t.Fatal(reason)
	}
	defer release()
	if err = source.Queue.BeginDispatch(t.Context(), first.ID); err != nil {
		t.Fatal(err)
	}
	if err = source.Queue.Finish(t.Context(), first.ID, triggerqueue.Dispatched, runID, "", now); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case <-ticks:
	case <-time.After(2 * time.Second):
		t.Fatal("initial evaluation did not complete")
	}
	workerPending(t, source, 0)
	release()
	select {
	case <-ticks:
	case <-time.After(2 * time.Second):
		t.Fatal("release did not wake retained demand")
	}
	workerPending(t, source, 1)
	if counter.calls != 1 {
		t.Fatal("capacity release repolled sealed observation", counter.calls)
	}
}
