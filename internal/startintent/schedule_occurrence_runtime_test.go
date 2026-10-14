package startintent

import (
	"fmt"
	"path/filepath"
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
