package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestSourceScheduleReopenExecutesCapturedStageOnce(t *testing.T) {
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
	first := openSourceDaemonFixture(t, layout)
	base := time.Now().UTC().Truncate(time.Minute).Add(-3 * time.Hour)
	due := base.Add(3 * time.Hour)
	if err := first.scheduler.ReconcileAll(nil, base); err != nil {
		t.Fatal(err)
	}
	first.scheduler.Tick(t.Context(), due)
	pending, err := first.service.queue.Pending(t.Context(), 10)
	if err != nil || len(pending) != 1 {
		t.Fatal(pending, err)
	}
	accepted := pending[0]
	envelope, err := startintent.Parse(accepted.Payload)
	if err != nil || envelope.Source == nil || !envelope.Source.ScheduledFrom.Equal(base) {
		t.Fatal(envelope, err)
	}
	if !envelope.Source.ScheduleWindowFrom.Equal(due.Add(-time.Hour)) || envelope.Source.ScheduleFireCount != 60 {
		t.Fatal("unbounded catch-up metadata", envelope.Source)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("execution preceded queue dispatch: %v", err)
	}
	first.close()
	// The current definition would write a different marker; acceptance keeps the old one.
	replacement := strings.Replace(workflow, strconv.Quote(marker), strconv.Quote(marker+".replacement"), 1)
	if replacement == workflow {
		t.Fatal("marker replacement did not change source")
	}
	writeFileContent(t, path, replacement)
	restarted := openSourceDaemonFixture(t, layout)
	if restarted.setup.ExecutionGeneration == envelope.Target.ConfigGeneration {
		t.Fatal("configuration generation unchanged")
	}
	if err := restarted.scheduler.ReconcileAll(nil, base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	restarted.scheduler.Tick(t.Context(), due)
	pending, err = restarted.service.queue.Pending(t.Context(), 10)
	if err != nil || len(pending) != 1 || pending[0].ID != accepted.ID {
		t.Fatal(pending, err)
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
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("captured stage did not write original marker: %v", err)
	}
	if _, err := os.Stat(marker + ".replacement"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement stage executed: %v", err)
	}
	dir, err := layout.FindRunDir(record.RunID)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if err := startintent.VerifyIdentity(identity, record); err != nil {
		t.Fatal(err)
	}
	phase, err := reader.Phase()
	if err != nil || phase != journal.PhaseCompleted {
		t.Fatal(phase, err)
	}
	restarted.scheduler.Tick(t.Context(), due)
	if pending, err := restarted.service.queue.Pending(t.Context(), 10); err != nil || len(pending) != 0 {
		t.Fatal(pending, err)
	}
	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	starts := 0
	for _, event := range events {
		if event.Type == journal.EventRunStarted {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("replayed firing produced %d runs", starts)
	}
}
