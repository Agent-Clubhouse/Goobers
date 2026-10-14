package startintent

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestIndependentDemandRetainsEveryGenerationAndTransfersExactOccurrence(t *testing.T) {
	source, entry := sourceFixture(t)
	base := time.Now().UTC().Truncate(time.Hour)
	schedule, err := localscheduler.ParseSchedule("@every 1h")
	if err != nil {
		t.Fatal(err)
	}
	entry.Schedules = []localscheduler.Schedule{schedule}
	if _, _, err = source.ScheduleCursor(t.Context(), entry, base, false, base); err != nil {
		t.Fatal(err)
	}
	revision, err := localscheduler.ScheduleRevision(entry.Schedules)
	if err != nil {
		t.Fatal(err)
	}
	var selected localscheduler.QueuedScheduleDemand
	for i := range 101 {
		entry.ConfigGeneration = fmt.Sprintf("generation-%03d", i)
		from := base.Add(time.Duration(i) * time.Hour)
		provenance := localscheduler.SourceTrigger{ScheduledFrom: from, ScheduledAt: from.Add(time.Hour)}
		raw, release, err := source.pin(t.Context(), entry, provenance)
		if err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("occurrence-%03d", i)
		err = source.Queue.CaptureScheduleDemand(t.Context(), triggerqueue.ScheduleDemandCapture{ID: id, Advance: triggerqueue.SourceAdvance{Scope: scheduleScope(entry), Revision: revision, Before: from, After: provenance.ScheduledAt}, Payload: raw, Independent: true, AcceptedAt: provenance.ScheduledAt})
		release()
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			selected = localscheduler.QueuedScheduleDemand{ID: id, Source: provenance, Count: 2}
		}
	}
	pins, err := RetainedGenerations(t.Context(), source.Queue)
	if err != nil || len(pins) != 101 {
		t.Fatal("retention lost another occurrence", len(pins), err)
	}
	if err = source.ObserveDemand(t.Context(), selected.ID, 2); err != nil {
		t.Fatal(err)
	}
	// Select a non-head occurrence explicitly, then edit the current definition.
	// Transfer must read that exact ID, retain its pins and reject crossed scope.
	entry.ConfigGeneration = "edited-current-generation"
	observation := localscheduler.WorkerObservation{At: base.Add(102 * time.Hour), Count: 1, MaxPending: 2}
	crossed := entry
	crossed.Gaggle = "other"
	if err = source.AcceptDemandWorkers(t.Context(), crossed, selected, observation); err == nil {
		t.Fatal("occurrence transferred across gaggle")
	}
	if err = source.AcceptDemandWorkers(t.Context(), entry, selected, observation); err != nil {
		t.Fatal(err)
	}
	pending, err := source.Queue.Pending(t.Context(), 10)
	if err != nil || len(pending) != 1 {
		t.Fatal(pending, err)
	}
	envelope, err := Parse(pending[0].Payload)
	if err != nil || envelope.Target.ConfigGeneration != "generation-001" || envelope.Source.ScheduleOrdinal != 1 || envelope.Source.ScheduleCount != 2 {
		t.Fatal(envelope, err)
	}
	selected.Queued = 1
	if err = source.AcceptDemandWorkers(t.Context(), entry, selected, observation); err != nil {
		t.Fatal(err)
	}
	oldest, err := source.Queue.ScheduleDemand(t.Context(), scheduleScope(entry))
	if err != nil || oldest.ID != "occurrence-000" || oldest.Count != -1 {
		t.Fatal("transfer consumed head occurrence", oldest, err)
	}
	// After full transfer, start receipts carry the pins until their runs settle.
	pins, err = RetainedGenerations(t.Context(), source.Queue)
	if err != nil || len(pins) != 101 || !pins["generation-001"] {
		t.Fatal("transfer dropped retained generation", len(pins), err)
	}
}

func TestIndependentDemandSizingUsesOldestCapturedDefinition(t *testing.T) {
	source, entry, counter, base := demandFixture(t)
	if _, _, err := source.ScheduleCursor(t.Context(), entry, base, false, base); err != nil {
		t.Fatal(err)
	}
	if err := source.CaptureDemand(t.Context(), entry, base, base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	captured := entry
	entry.ConfigGeneration = "new-generation"
	revision, err := localscheduler.ScheduleRevision(entry.Schedules)
	if err != nil {
		t.Fatal(err)
	}
	provenance := localscheduler.SourceTrigger{ScheduledFrom: base.Add(time.Hour), ScheduledAt: base.Add(2 * time.Hour)}
	raw, release, err := source.pin(t.Context(), entry, provenance)
	if err != nil {
		t.Fatal(err)
	}
	err = source.Queue.CaptureScheduleDemand(t.Context(), triggerqueue.ScheduleDemandCapture{ID: "next-independent", Advance: triggerqueue.SourceAdvance{Scope: scheduleScope(entry), Revision: revision, Before: provenance.ScheduledFrom, After: provenance.ScheduledAt}, Payload: raw, Independent: true, AcceptedAt: provenance.ScheduledAt})
	release()
	if err != nil {
		t.Fatal(err)
	}
	loads := 0
	source.Build = func(_ context.Context, target Target) (Prepared, error) {
		loads++
		if target.ConfigGeneration != captured.ConfigGeneration {
			t.Fatal("sizing used newer generation", target)
		}
		return Prepared{Entry: captured, Release: func() {}}, nil
	}
	demand, release, err := source.LoadDemand(t.Context(), entry)
	if err != nil || demand == nil || demand.Pinned == nil || demand.Pinned.ScheduleDemandCounter != counter {
		t.Fatal(demand, err)
	}
	release()
	if err = source.ObserveDemand(t.Context(), demand.ID, 3); err != nil {
		t.Fatal(err)
	}
	sealed, release, err := source.LoadDemand(t.Context(), entry)
	if release != nil {
		release()
	}
	if err != nil || sealed.Count != 3 || loads != 1 {
		t.Fatal("sealed occurrence was resized", sealed, loads, err)
	}
}
