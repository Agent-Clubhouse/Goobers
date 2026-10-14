package startintent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestOccurrenceDiscoveryRestartsAtSimultaneousSweepBoundary(t *testing.T) {
	source, entry := sourceFixture(t)
	base := time.Now().UTC().Truncate(time.Hour)
	queuePath := filepath.Join(t.TempDir(), "occurrences.db")
	var err error
	source.Queue, err = triggerqueue.Open(queuePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Queue.Close() })
	entry.ScheduleCatchUpAll = true
	schedule, err := localscheduler.ParseSchedule("@every 1m")
	if err != nil {
		t.Fatal(err)
	}
	entry.Schedules = []localscheduler.Schedule{schedule, schedule, schedule}
	log, _, err := journal.OpenInstanceLog(filepath.Join(t.TempDir(), "journal"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	scheduler := localscheduler.New([]localscheduler.WorkflowEntry{entry}, log, localscheduler.WithSourceQueue(source))
	if err = scheduler.ReconcileAll(nil, base); err != nil {
		t.Fatal(err)
	}
	now := base.Add(34 * time.Minute)
	scheduler.Tick(t.Context(), now)
	pending, err := source.Queue.Pending(t.Context(), 100)
	if err != nil || len(pending) != 100 {
		t.Fatal(len(pending), err)
	}
	seen := map[string]bool{}
	drain := func() {
		t.Helper()
		for _, record := range pending {
			envelope, err := Parse(record.Payload)
			if err != nil {
				t.Fatal(err)
			}
			key := fmt.Sprintf("%d/%s", envelope.Source.ScheduleDeclaration, envelope.Source.ScheduledAt)
			if seen[key] || envelope.Source.ScheduledAt.After(now) {
				t.Fatal("repeated or future", key)
			}
			seen[key] = true
			if err = source.Queue.BeginDispatch(t.Context(), record.ID); err != nil {
				t.Fatal(err)
			}
			if err = source.Queue.Finish(t.Context(), record.ID, triggerqueue.Dispatched, "run-"+record.ID, "", now); err != nil {
				t.Fatal(err)
			}
		}
	}
	drain()
	if err = source.Queue.Close(); err != nil {
		t.Fatal(err)
	}
	source.Queue, err = triggerqueue.Open(queuePath)
	if err != nil {
		t.Fatal(err)
	}
	scheduler = localscheduler.New([]localscheduler.WorkflowEntry{entry}, log, localscheduler.WithSourceQueue(source))
	if err = scheduler.ReconcileAll(nil, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	scheduler.Tick(t.Context(), now)
	pending, err = source.Queue.Pending(t.Context(), 100)
	if err != nil || len(pending) != 2 {
		t.Fatal(len(pending), err)
	}
	drain()
	if len(seen) != 102 {
		t.Fatal(len(seen))
	}
	scheduler.Tick(t.Context(), now)
	if pending, err = source.Queue.Pending(t.Context(), 100); err != nil || len(pending) != 0 {
		t.Fatal(len(pending), err)
	}
}

func TestOccurrenceDiscoveryPreservesMixedIntervalPhase(t *testing.T) {
	source, entry := sourceFixture(t)
	base := time.Now().UTC().Truncate(time.Hour)
	entry.ScheduleCatchUpAll = true
	for _, expression := range []string{"@every 2m", "@every 3m"} {
		schedule, err := localscheduler.ParseSchedule(expression)
		if err != nil {
			t.Fatal(err)
		}
		entry.Schedules = append(entry.Schedules, schedule)
	}
	log, _, err := journal.OpenInstanceLog(filepath.Join(t.TempDir(), "journal"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	scheduler := localscheduler.New([]localscheduler.WorkflowEntry{entry}, log, localscheduler.WithSourceQueue(source))
	if err = scheduler.ReconcileAll(nil, base); err != nil {
		t.Fatal(err)
	}
	scheduler.Tick(t.Context(), base.Add(12*time.Minute))
	pending, err := source.Queue.Pending(t.Context(), 100)
	if err != nil || len(pending) != 10 {
		t.Fatal(len(pending), err)
	}
	for _, record := range pending {
		envelope, err := Parse(record.Payload)
		if err != nil {
			t.Fatal(err)
		}
		step := time.Duration(envelope.Source.ScheduleDeclaration+1) * time.Minute
		if envelope.Source.ScheduledAt.Sub(base)%step != 0 || envelope.Source.ScheduledAt.Sub(envelope.Source.ScheduledFrom) != step {
			t.Fatal(envelope.Source)
		}
	}
}

func TestOccurrenceDemandCaptureRetainsEveryFireBeforeSizing(t *testing.T) {
	source, entry := sourceFixture(t)
	base := time.Now().UTC().Truncate(time.Hour)
	entry.ScheduleCatchUpAll = true
	entry.ScheduleDemandCounter = workerCount(3)
	schedule, err := localscheduler.ParseSchedule("@every 1m")
	if err != nil {
		t.Fatal(err)
	}
	entry.Schedules = []localscheduler.Schedule{schedule}
	positions, _, err := source.ScheduleOccurrences(t.Context(), entry, base, false, base)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		at := base.Add(time.Duration(i+1) * time.Minute)
		if err = source.AcceptOccurrence(t.Context(), entry, 0, positions[0], at, base.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		positions[0] = at
	}
	records, err := source.Queue.ScheduleDemandPage(t.Context(), "", 100)
	if err != nil || len(records) != 3 {
		t.Fatal(records, err)
	}
	for _, record := range records {
		if !record.Independent || record.Count != -1 {
			t.Fatal(record)
		}
	}
	selected := records[1]
	if err = source.ObserveDemand(t.Context(), selected.ID, 2); err != nil {
		t.Fatal(err)
	}
	envelope, err := Parse(selected.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if err = source.AcceptDemandWorkers(t.Context(), entry, localscheduler.QueuedScheduleDemand{ID: selected.ID, Source: *envelope.Source, Count: 2}, localscheduler.WorkerObservation{At: base.Add(time.Hour), Count: 2, MaxPending: 2}); err != nil {
		t.Fatal(err)
	}
	pending, err := source.Queue.Pending(t.Context(), 10)
	if err != nil || len(pending) != 2 {
		t.Fatal(pending, err)
	}
	for _, record := range pending {
		got, err := Parse(record.Payload)
		if err != nil || got.Source.ScheduleDeclaration != 1 || !got.Source.ScheduledAt.Equal(envelope.Source.ScheduledAt) {
			t.Fatal(got, err)
		}
	}
}

func TestOccurrenceAdmissionRetainsBackedOffFireAndReservedIdentity(t *testing.T) {
	source, entry := sourceFixture(t)
	base := time.Now().UTC().Truncate(time.Minute)
	now := base
	schedule, err := localscheduler.ParseSchedule("@every 1m")
	if err != nil {
		t.Fatal(err)
	}
	entry.Schedules = []localscheduler.Schedule{schedule}
	entry.ScheduleCatchUpAll = true
	entry.ScheduleBackoffs = []localscheduler.IdleBackoffConfig{{Enabled: true, Floor: time.Minute, Ceiling: 4 * time.Minute}}
	started := make(chan string, 4)
	entry.Starter = scheduleStarterFunc(func(_ context.Context, req localscheduler.StartRequest) (localscheduler.StartResult, error) {
		started <- req.RunID
		return localscheduler.StartResult{Phase: journal.PhaseCompleted, NoWork: true}, nil
	})
	log, _, err := journal.OpenInstanceLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	scheduler := localscheduler.New([]localscheduler.WorkflowEntry{entry}, log, localscheduler.WithSourceQueue(source), localscheduler.WithClock(func() time.Time { return now }, time.After))
	defer scheduler.Wait()
	if err = scheduler.ReconcileAll(nil, base); err != nil {
		t.Fatal(err)
	}
	service := &Service{Queue: source.Queue, Now: func() time.Time { return now }, Scheduler: func() *localscheduler.Scheduler { return scheduler }, RunDirectory: func(context.Context, string) (string, error) { return "", nil }, Build: func(context.Context, Target) (Prepared, error) {
		return Prepared{Entry: entry, Release: func() {}}, nil
	}}
	for minute := 1; minute <= 2; minute++ {
		now = base.Add(time.Duration(minute) * time.Minute)
		scheduler.Tick(t.Context(), now)
		pending, err := source.Queue.Pending(t.Context(), 10)
		if err != nil || len(pending) != 1 {
			t.Fatal(pending, err)
		}
		if err = service.Dispatch(t.Context(), t.Context(), pending[0]); err != nil {
			t.Fatal(err)
		}
		scheduler.Wait()
		<-started
	}
	now = base.Add(3 * time.Minute)
	scheduler.Tick(t.Context(), now)
	pending, err := source.Queue.Pending(t.Context(), 10)
	if err != nil || len(pending) != 1 {
		t.Fatal("backoff lost occurrence during discovery", pending, err)
	}
	original := pending[0]
	if err = service.Dispatch(t.Context(), t.Context(), original); err != nil {
		t.Fatal(err)
	}
	held, err := source.Queue.Get(t.Context(), original.ID, "scheduler")
	if err != nil || held.State != triggerqueue.Accepted || !strings.HasPrefix(held.Reason, localscheduler.ReasonScheduleBackoff) || string(held.Payload) != string(original.Payload) {
		t.Fatal(held, err)
	}
	select {
	case id := <-started:
		t.Fatal("started through backoff", id)
	default:
	}
	now = base.Add(4 * time.Minute)
	if err = service.Dispatch(t.Context(), t.Context(), held); err != nil {
		t.Fatal(err)
	}
	scheduler.Wait()
	select {
	case id := <-started:
		if id != strings.TrimPrefix(original.ID, "trigger-") {
			t.Fatal("identity changed", id, original.ID)
		}
	default:
		t.Fatal("did not resume")
	}
	resumed, err := source.Queue.Get(t.Context(), original.ID, "scheduler")
	if err != nil || resumed.State != triggerqueue.Dispatching || resumed.RunID != strings.TrimPrefix(original.ID, "trigger-") || resumed.Reason != "" {
		t.Fatal(resumed, err)
	}
}

func TestOccurrencePolicyChangeKeepsOldCoalescedCustodyAndResetsOnlyFutureDiscovery(t *testing.T) {
	source, entry := sourceFixture(t)
	base := time.Now().UTC().Truncate(time.Minute)
	schedule, err := localscheduler.ParseSchedule("@every 1m")
	if err != nil {
		t.Fatal(err)
	}
	entry.Schedules = []localscheduler.Schedule{schedule}
	if _, _, err = source.ScheduleCursor(t.Context(), entry, base, false, base); err != nil {
		t.Fatal(err)
	}
	if err = source.AcceptSchedule(t.Context(), entry, base, base.Add(2*time.Minute), true); err != nil {
		t.Fatal(err)
	}
	old, err := source.Queue.Pending(t.Context(), 10)
	if err != nil || len(old) != 1 {
		t.Fatal(old, err)
	}
	entry.ScheduleCatchUpAll = true
	observed := base.Add(3 * time.Minute)
	positions, legacy, err := source.ScheduleOccurrences(t.Context(), entry, base, false, observed)
	if err != nil || legacy || len(positions) != 1 || !positions[0].Equal(observed) {
		t.Fatal(positions, legacy, err)
	}
	if err = source.AcceptOccurrence(t.Context(), entry, 0, observed, observed.Add(time.Minute), observed.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	now, err := source.Queue.Pending(t.Context(), 10)
	if err != nil || len(now) != 2 {
		t.Fatal(now, err)
	}
	retained, err := source.Queue.Get(t.Context(), old[0].ID, "scheduler")
	if err != nil || string(retained.Payload) != string(old[0].Payload) {
		t.Fatal(retained, err)
	}
}

func TestOccurrenceDemandSurvivesRemovalOfCurrentSizingPolicy(t *testing.T) {
	for _, sealed := range []bool{false, true} {
		t.Run(fmt.Sprint(sealed), func(t *testing.T) {
			source, entry, counter, base := demandFixture(t)
			entry.ScheduleCatchUpAll = true
			captured := entry
			source.Build = func(_ context.Context, target Target) (Prepared, error) {
				if target.ConfigGeneration != captured.ConfigGeneration {
					t.Fatal(target)
				}
				return Prepared{Entry: captured, Release: func() {}}, nil
			}
			positions, _, err := source.ScheduleOccurrences(t.Context(), entry, base, false, base)
			if err != nil {
				t.Fatal(err)
			}
			if err = source.AcceptOccurrence(t.Context(), entry, 0, positions[0], base.Add(time.Hour), base.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			records, err := source.Queue.ScheduleDemandPage(t.Context(), "", 100)
			if err != nil || len(records) != 1 {
				t.Fatal(records, err)
			}
			if sealed {
				if err = source.ObserveDemand(t.Context(), records[0].ID, 2); err != nil {
					t.Fatal(err)
				}
			}
			entry.ScheduleCatchUpAll = false
			entry.ScheduleDemandCounter = nil
			entry.ConfigGeneration = "edited"
			log, _, err := journal.OpenInstanceLog(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = log.Close() }()
			scheduler := localscheduler.New([]localscheduler.WorkflowEntry{entry}, log, localscheduler.WithSourceQueue(source))
			if err = scheduler.ReconcileAll(nil, base.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			scheduler.Tick(t.Context(), base.Add(time.Hour))
			pending, err := source.Queue.Pending(t.Context(), 10)
			if err != nil || len(pending) != 2 {
				t.Fatal("accepted demand stranded after policy edit", len(pending), err)
			}
			if (counter.calls == 0) != sealed {
				t.Fatal("sealed count recounted or unsealed count not sized", counter.calls, sealed)
			}
			for _, record := range pending {
				envelope, err := Parse(record.Payload)
				if err != nil || envelope.Target.ConfigGeneration != captured.ConfigGeneration || envelope.Source.ScheduleDeclaration != 1 {
					t.Fatal(envelope, err)
				}
			}
		})
	}
}

func TestOccurrenceDiscoveryWhilePausedRetainsPinsAndResumesWithCurrentPolicy(t *testing.T) {
	source, entry := sourceFixture(t)
	base := time.Now().UTC().Truncate(time.Minute)
	entry.ScheduleCatchUpAll = true
	entry.DisabledReason = "workflow paused"
	schedule, err := localscheduler.ParseSchedule("@every 1m")
	if err != nil {
		t.Fatal(err)
	}
	entry.Schedules = []localscheduler.Schedule{schedule}
	started := make(chan string, 2)
	entry.Starter = scheduleStarterFunc(func(_ context.Context, req localscheduler.StartRequest) (localscheduler.StartResult, error) {
		started <- req.RunID
		return localscheduler.StartResult{Phase: journal.PhaseCompleted}, nil
	})
	archived := entry
	log, _, err := journal.OpenInstanceLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	scheduler := localscheduler.New([]localscheduler.WorkflowEntry{entry}, log, localscheduler.WithSourceQueue(source))
	if err = scheduler.ReconcileAll(nil, base); err != nil {
		t.Fatal(err)
	}
	scheduler.Tick(t.Context(), base.Add(2*time.Minute))
	pending, err := source.Queue.Pending(t.Context(), 10)
	if err != nil || len(pending) != 2 {
		t.Fatal("pause discarded discovery", len(pending), err)
	}
	service := &Service{Queue: source.Queue, Now: func() time.Time { return base.Add(2 * time.Minute) }, Scheduler: func() *localscheduler.Scheduler { return scheduler }, RunDirectory: func(context.Context, string) (string, error) { return "", nil }, Build: func(context.Context, Target) (Prepared, error) {
		return Prepared{Entry: archived, Release: func() {}}, nil
	}}
	if err = service.Dispatch(t.Context(), t.Context(), pending[0]); err != nil {
		t.Fatal(err)
	}
	held, err := source.Queue.Get(t.Context(), pending[0].ID, "scheduler")
	if err != nil || held.State != triggerqueue.Accepted || !strings.HasPrefix(held.Reason, localscheduler.ReasonDisabled) {
		t.Fatal(held, err)
	}
	entry.DisabledReason = ""
	entry.ConfigGeneration = "enabled-generation"
	scheduler = localscheduler.New([]localscheduler.WorkflowEntry{entry}, log, localscheduler.WithSourceQueue(source))
	if err = scheduler.ReconcileAll(nil, base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err = service.Dispatch(t.Context(), t.Context(), held); err != nil {
		t.Fatal(err)
	}
	scheduler.Wait()
	select {
	case id := <-started:
		if id != strings.TrimPrefix(held.ID, "trigger-") {
			t.Fatal(id)
		}
	default:
		t.Fatal("archived pause blocked current permission")
	}
}

func TestOccurrencePauseCapturesDemandWithoutProviderSizing(t *testing.T) {
	source, entry, counter, base := demandFixture(t)
	entry.ScheduleCatchUpAll = true
	entry.DisabledReason = "workflow paused"
	log, _, err := journal.OpenInstanceLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	scheduler := localscheduler.New([]localscheduler.WorkflowEntry{entry}, log, localscheduler.WithSourceQueue(source))
	if err = scheduler.ReconcileAll(nil, base); err != nil {
		t.Fatal(err)
	}
	scheduler.Tick(t.Context(), base.Add(2*time.Hour))
	records, err := source.Queue.ScheduleDemandPage(t.Context(), "", 100)
	if err != nil || len(records) != 2 || counter.calls != 0 {
		t.Fatal(records, counter.calls, err)
	}
	for _, record := range records {
		if record.Count != -1 {
			t.Fatal("paused demand was sized", record)
		}
	}
}
