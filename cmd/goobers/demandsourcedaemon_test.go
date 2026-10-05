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

// Replace only provider sizing. Both archived builds and actual starts still
// traverse the production generation compiler, queue, scheduler, and Runner.
func TestDemandSourceHostPinsBeforeSizingAndStartsActualRunner(t *testing.T) {
	f := sourceHost(t, "    - type: schedule\n      schedule: \"@every 1m\"")
	f.entry.ScheduleDemandCounter = queuedHostCount(2)
	sources := f.setup.SourceStarts.(*startintent.Sources)
	build := sources.Build
	builds := 0
	sources.Build = func(ctx context.Context, target startintent.Target) (startintent.Prepared, error) {
		builds++
		retained, err := f.service.queue.ScheduleDemandPage(ctx, "", 100)
		if err != nil || len(retained) != 1 {
			t.Fatal("counter built before durable custody", retained, err)
		}
		prepared, err := build(ctx, target)
		if err == nil {
			prepared.Entry.ScheduleDemandCounter = queuedHostCount(2)
		}
		return prepared, err
	}
	log, _, err := journal.OpenInstanceLog(filepath.Join(t.TempDir(), "demand-scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	f.sched = localscheduler.New([]localscheduler.WorkflowEntry{f.entry}, log, localscheduler.WithSourceQueue(f.setup.SourceStarts))
	f.service.dispatch.AttachScheduler(f.sched)
	if err = f.sched.ReconcileAll(nil, f.now); err != nil {
		t.Fatal(err)
	}
	due := f.now.Add(time.Minute)
	f.sched.Tick(t.Context(), due)
	records, err := f.service.queue.Pending(t.Context(), 100)
	if err != nil || len(records) != 1 {
		t.Fatal(records, err)
	}
	pins, err := startintent.RetainedGenerations(t.Context(), f.service.queue)
	if err != nil || !pins[f.generation] {
		t.Fatal(pins, err)
	}
	writeFileContent(t, f.source, "unsaved invalid source: [")
	all := append([]triggerqueue.Record(nil), records...)
	for i := 0; i < 2; i++ {
		f.drain(t)
		f.sched.Wait()
		f.wg.Wait()
		f.drain(t)
		if i == 0 {
			f.sched.Tick(t.Context(), due.Add(time.Second))
			records, err = f.service.queue.Pending(t.Context(), 100)
			if err != nil || len(records) != 1 {
				t.Fatal(records, err)
			}
			all = append(all, records...)
		}
	}
	if builds != 1 {
		t.Fatal("sealed count rebuilt/re-polled", builds)
	}
	for _, record := range all {
		done, err := f.service.queue.Get(t.Context(), record.ID, "scheduler")
		if err != nil || done.State != triggerqueue.Dispatched {
			t.Fatal(done, err)
		}
		dir, err := f.layout.FindRunDir(strings.TrimPrefix(record.ID, "trigger-"))
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
	pending, err := f.service.queue.ScheduleDemandPage(t.Context(), "", 100)
	if err != nil || len(pending) != 0 {
		t.Fatal(pending, err)
	}
}
