package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Only the external provider count is replaced. Capture, archive compilation,
// scheduler admission, runner execution and journal observation are production.
type queuedHostCount int

func (c queuedHostCount) EligibleCount(context.Context) (int, error) { return int(c), nil }

func TestWorkerSourceHostBacklogQueuesBeforeActualPinnedRunner(t *testing.T) {
	f := sourceHost(t, "    - type: backlog-item")
	if f.entry.BacklogCounter == nil {
		t.Fatal("configured backlog source absent")
	}
	f.entry.BacklogCounter = queuedHostCount(5)
	f.entry.Readiness.MaxConcurrentRuns = 3
	log, _, err := journal.OpenInstanceLog(filepath.Join(t.TempDir(), "worker-source-scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	f.sched = localscheduler.New([]localscheduler.WorkflowEntry{f.entry}, log, localscheduler.WithSourceQueue(f.setup.SourceStarts))
	f.service.dispatch.AttachScheduler(f.sched)
	now := time.Now().UTC()
	f.sched.Tick(t.Context(), now)
	records, err := f.service.queue.Pending(t.Context(), 100)
	if err != nil || len(records) != 3 {
		t.Fatal(records, err)
	}
	for _, record := range records {
		if _, err = f.layout.FindRunDir(strings.TrimPrefix(record.ID, "trigger-")); err == nil {
			t.Fatal("execution preceded queue commit")
		}
		e, err := startintent.Parse(record.Payload)
		if err != nil || e.Source == nil || e.Source.WorkerKind != "backlog" || e.Source.ObservedCount != 5 || e.Target.ConfigGeneration != f.generation {
			t.Fatal(e, err)
		}
	}
	// The saved generation, not an unsaved edit, controls these accepted starts.
	writeFileContent(t, f.source, "invalid pending YAML: [")
	f.drain(t)
	f.sched.Wait()
	f.wg.Wait()
	f.drain(t)
	for _, record := range records {
		done, err := f.service.queue.Get(t.Context(), record.ID, "scheduler")
		if err != nil || done.State != triggerqueue.Dispatched {
			t.Fatal(done, err)
		}
		dir, err := f.layout.FindRunDir(done.RunID)
		if err != nil {
			t.Fatal(err)
		}
		reader, err := journal.OpenReadOnly(dir)
		if err != nil {
			t.Fatal(err)
		}
		id, err := reader.Identity()
		if err != nil {
			t.Fatal(err)
		}
		if err = startintent.VerifyIdentity(id, record); err != nil {
			t.Fatal(err)
		}
		phase, err := reader.Phase()
		if err != nil || phase != journal.PhaseCompleted {
			t.Fatal(phase, err)
		}
	}
}
