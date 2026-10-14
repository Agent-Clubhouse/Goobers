package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type sourceScheduleCounter struct{ calls int }

func (c *sourceScheduleCounter) EligibleCount(context.Context) (int, error) { c.calls++; return 2, nil }

// Only provider sizing is substituted. Archive capture, durable admission,
// daemon teardown/reopen, the compiler, and runner are the production path.
func TestSourceScheduleDemandReopenRunsRemainingCapturedWorkers(t *testing.T) {
	root, marker := initRunDurationDemo(t)
	layout := instance.NewLayout(root)
	path := filepath.Join(layout.ConfigDir(), "gaggles", "example", "workflows", "default-implement.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	workflow := strings.Replace(string(raw), "    - type: manual", "    - type: schedule\n      schedule: \"@every 1m\"", 1)
	if workflow == string(raw) {
		t.Fatal("missing manual fixture")
	}
	writeFileContent(t, path, workflow)
	counter := &sourceScheduleCounter{}
	configure := func(host *sourceDaemonFixture) {
		entries := append([]localscheduler.WorkflowEntry(nil), host.setup.Entries...)
		for i := range entries {
			entries[i].ScheduleDemandCounter = counter
			entries[i].Readiness.MaxConcurrentRuns = 1
		}
		host.scheduler = localscheduler.New(entries, host.setup.InstanceLog, host.setup.SchedulerOptions()...)
		host.service.dispatch.AttachScheduler(host.scheduler)
	}
	first := openSourceDaemonFixture(t, layout)
	configure(&first)
	originalBuild := first.setup.SourceStarts.Build
	first.setup.SourceStarts.Build = func(ctx context.Context, target startintent.Target) (startintent.Prepared, error) {
		demands, err := first.service.queue.ScheduleDemandPage(ctx, "", 100)
		if err != nil || len(demands) != 1 || demands[0].Count != -1 {
			t.Fatalf("sizing preceded durable capture: %v %v", demands, err)
		}
		prepared, err := originalBuild(ctx, target)
		prepared.Entry.ScheduleDemandCounter = counter
		return prepared, err
	}
	base := time.Now().UTC().Truncate(time.Minute).Add(-2 * time.Minute)
	due := base.Add(time.Minute)
	if err := first.scheduler.ReconcileAll(nil, base); err != nil {
		t.Fatal(err)
	}
	first.scheduler.Tick(t.Context(), due)
	pending, err := first.service.queue.Pending(t.Context(), 10)
	if err != nil || len(pending) != 1 || counter.calls != 1 {
		t.Fatal(pending, counter.calls, err)
	}
	original, err := startintent.Parse(pending[0].Payload)
	if err != nil {
		t.Fatal(err)
	}
	first.close()
	replacement := strings.Replace(workflow, strconv.Quote(marker), strconv.Quote(marker+".replacement"), 1)
	if replacement == workflow {
		t.Fatal("marker replacement unchanged")
	}
	replacement = strings.Replace(replacement, "@every 1m", "@every 5m", 1)
	writeFileContent(t, path, replacement)
	restarted := openSourceDaemonFixture(t, layout)
	configure(&restarted)
	restarted.setup.SourceStarts.Build = func(context.Context, startintent.Target) (startintent.Prepared, error) {
		t.Fatal("sealed count rebuilt or repolled after restart")
		return startintent.Prepared{}, nil
	}
	if restarted.setup.ExecutionGeneration == original.Target.ConfigGeneration {
		t.Fatal("source generation unchanged")
	}
	if err := restarted.scheduler.ReconcileAll(nil, base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for ordinal := 1; ordinal <= 2; ordinal++ {
		restarted.scheduler.Tick(t.Context(), due.Add(time.Duration(ordinal)*time.Second))
		records, err := restarted.service.queue.Pending(t.Context(), 10)
		if err != nil || len(records) != 1 {
			t.Fatal(records, err)
		}
		accepted := records[0]
		envelope, err := startintent.Parse(accepted.Payload)
		if err != nil || envelope.Target.ConfigGeneration != original.Target.ConfigGeneration || envelope.Source.ScheduleOrdinal != ordinal || envelope.Source.ScheduleCount != 2 {
			t.Fatal(envelope, err)
		}
		if err := restarted.service.Drain(t.Context()); err != nil {
			t.Fatal(err)
		}
		restarted.scheduler.Wait()
		restarted.workers.Wait()
		if err := restarted.service.Drain(t.Context()); err != nil {
			t.Fatal(err)
		}
		record, err := restarted.service.queue.Get(t.Context(), accepted.ID, "scheduler")
		if err != nil || record.State != triggerqueue.Dispatched {
			t.Fatal(record, err)
		}
		dir, err := layout.FindRunDir(record.RunID)
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
		if err := startintent.VerifyIdentity(id, record); err != nil {
			t.Fatal(err)
		}
		phase, err := reader.Phase()
		if err != nil || phase != journal.PhaseCompleted {
			t.Fatal(phase, err)
		}
	}
	if counter.calls != 1 {
		t.Fatalf("count observed %d times", counter.calls)
	}
	if demands, err := restarted.service.queue.ScheduleDemandPage(t.Context(), "", 100); err != nil || len(demands) != 0 {
		t.Fatal(demands, err)
	}
	restarted.scheduler.Tick(t.Context(), due.Add(3*time.Second))
	if records, err := restarted.service.queue.Pending(t.Context(), 10); err != nil || len(records) != 0 {
		t.Fatal(records, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker + ".replacement"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement executed: %v", err)
	}
}
