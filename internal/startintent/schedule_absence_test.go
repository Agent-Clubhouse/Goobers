package startintent

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestScheduleRemovalAndReenableDoesNotDiscoverAbsentPeriod(t *testing.T) {
	source, entry := sourceFixture(t)
	path := filepath.Join(t.TempDir(), "queue.db")
	var err error
	source.Queue, err = triggerqueue.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Queue.Close() })
	schedule, err := localscheduler.ParseSchedule("@every 1h")
	if err != nil {
		t.Fatal(err)
	}
	entry.Schedules = []localscheduler.Schedule{schedule}
	entry.ScheduleCatchUpAll = true
	base := time.Now().UTC().Truncate(time.Hour)
	log, _, err := journal.OpenInstanceLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	tick := func(current localscheduler.WorkflowEntry, at time.Time) {
		t.Helper()
		scheduler := localscheduler.New([]localscheduler.WorkflowEntry{current}, log, localscheduler.WithSourceQueue(source))
		if err := scheduler.ReconcileAll(nil, at); err != nil {
			t.Fatal(err)
		}
		scheduler.Tick(t.Context(), at)
	}
	tick(entry, base)
	tick(entry, base.Add(time.Hour))
	original, err := source.Queue.Pending(t.Context(), 100)
	if err != nil || len(original) != 1 {
		t.Fatal(len(original), err)
	}
	absent := entry
	absent.Schedules = nil
	absent.ScheduleCatchUpAll = false
	tick(absent, base.Add(2*time.Hour))
	if err = source.Queue.Close(); err != nil {
		t.Fatal(err)
	}
	source.Queue, err = triggerqueue.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	tick(entry, base.Add(4*time.Hour))
	pending, err := source.Queue.Pending(t.Context(), 100)
	if err != nil || len(pending) != 1 {
		t.Fatal("re-enabled schedule manufactured absent-period fires", len(pending), err)
	}
	if pending[0].ID != original[0].ID || string(pending[0].Payload) != string(original[0].Payload) {
		t.Fatal("configuration edit changed accepted custody")
	}
	tick(entry, base.Add(5*time.Hour))
	pending, err = source.Queue.Pending(t.Context(), 100)
	if err != nil || len(pending) != 2 {
		t.Fatal("future schedule did not resume", len(pending), err)
	}
}
