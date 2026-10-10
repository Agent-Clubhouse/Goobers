package startintent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

func TestSourceScheduleRecoversQueueCursorAndUsesPinnedStarter(t *testing.T) {
	source, entry := sourceFixture(t)
	base := time.Now().UTC().Truncate(time.Minute)
	schedule, err := localscheduler.ParseSchedule("@every 1m")
	if err != nil {
		t.Fatal(err)
	}
	entry.Schedules = []localscheduler.Schedule{schedule}
	log, _, err := journal.OpenInstanceLog(filepath.Join(t.TempDir(), "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	scheduler := localscheduler.New([]localscheduler.WorkflowEntry{entry}, log, localscheduler.WithSourceQueue(source))
	if err = scheduler.ReconcileAll(nil, base); err != nil {
		t.Fatal(err)
	}
	scheduler.Tick(t.Context(), base.Add(time.Minute))
	pending, err := source.Queue.Pending(t.Context(), 100)
	if err != nil || len(pending) != 1 {
		t.Fatal(pending, err)
	}
	accepted, err := Parse(pending[0].Payload)
	if err != nil || accepted.Source == nil || !accepted.Source.ScheduledFrom.Equal(base) {
		t.Fatal(accepted, err)
	}
	evaluations, err := localscheduler.ReadTriggerEvaluations(log.Dir())
	if err != nil || !evaluations[localscheduler.WorkflowIdentity{Gaggle: entry.Gaggle, Workflow: entry.Workflow}].Equal(base.Add(time.Minute)) {
		t.Fatal(evaluations, err)
	}

	// Simulate a stale/ahead legacy evaluation file; durable cursor controls restart.
	restarted := localscheduler.New([]localscheduler.WorkflowEntry{entry}, log, localscheduler.WithSourceQueue(source))
	if err = restarted.ReconcileAll(nil, base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	restarted.Tick(t.Context(), base.Add(time.Minute))
	pending, err = source.Queue.Pending(t.Context(), 100)
	if err != nil || len(pending) != 1 {
		t.Fatal(pending, err)
	}
	restarted.Tick(t.Context(), base.Add(5*time.Minute))
	pending, err = source.Queue.Pending(t.Context(), 100)
	if err != nil || len(pending) != 2 {
		t.Fatal(pending, err)
	}
	// Any number of missed ticks still produces one catch-up worker, as before.
	last, err := Parse(pending[1].Payload)
	if err != nil || !last.Source.ScheduledFrom.Equal(base.Add(time.Minute)) || !last.Source.ScheduledAt.Equal(base.Add(5*time.Minute)) {
		t.Fatal(last, err)
	}
}
func TestSourceScheduleArchiveFailureLeavesDueCursor(t *testing.T) {
	source, entry := sourceFixture(t)
	base := time.Now().UTC()
	if _, _, err := source.ScheduleCursor(t.Context(), entry, base, false); err != nil {
		t.Fatal(err)
	}
	source.Acquire = func(context.Context, Target) (func(), error) { return nil, errors.New("archive unavailable") }
	if err := source.AcceptSchedule(t.Context(), entry, base, base.Add(time.Minute), true); err == nil {
		t.Fatal("missing pins accepted")
	}
	if cursor, _, err := source.ScheduleCursor(t.Context(), entry, base.Add(time.Hour), false); err != nil || !cursor.Equal(base) {
		t.Fatal(cursor, err)
	}
}

func TestSourceScheduleAdoptsLegacyPendingFireOnce(t *testing.T) {
	source, entry := sourceFixture(t)
	schedule, err := localscheduler.ParseSchedule("@every 1h")
	if err != nil {
		t.Fatal(err)
	}
	entry.Schedules = []localscheduler.Schedule{schedule}
	dir := filepath.Join(t.TempDir(), "scheduler")
	log, _, err := journal.OpenInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	legacy := []byte(`{"workflows":[{"gaggle":"own","workflow":"worker"}]}`)
	writeLegacy := func() {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "schedule-demand.json"), legacy, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeLegacy()
	before := time.Now().UTC()
	scheduler := localscheduler.New([]localscheduler.WorkflowEntry{entry}, log, localscheduler.WithSourceQueue(source))
	if err := scheduler.ReconcileAll(nil, before); err != nil {
		t.Fatal(err)
	}
	scheduler.Tick(t.Context(), before.Add(time.Second)) // No new cron fire is due.
	pending, err := source.Queue.Pending(t.Context(), 10)
	if err != nil || len(pending) != 1 {
		t.Fatal(pending, err)
	}
	first := pending[0].ID
	writeLegacy() // Crash left the old auxiliary file even though queue commit succeeded.
	restarted := localscheduler.New([]localscheduler.WorkflowEntry{entry}, log, localscheduler.WithSourceQueue(source))
	if err := restarted.ReconcileAll(nil, before); err != nil {
		t.Fatal(err)
	}
	restarted.Tick(t.Context(), before.Add(2*time.Second))
	pending, err = source.Queue.Pending(t.Context(), 10)
	if err != nil || len(pending) != 1 || pending[0].ID != first {
		t.Fatal(pending, err)
	}
}
