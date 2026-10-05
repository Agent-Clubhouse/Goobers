package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// engine-start delegates selection to the daemon, including its ordinary local
// fallback. This drives that real command/file path through archive admission;
// it never substitutes a fake triggerer that could bypass durable acceptance.
func TestDelegatedEngineCommandAcceptsPinnedStartBeforeAcknowledgement(t *testing.T) {
	f := ordinaryHost(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- delegateEngineStart(ctx, f.layout, "example", "default-implement", &stdout, &stderr) }()
	var record triggerqueue.Record
	for record.ID == "" {
		if err := sweepPendingTriggersWithAdmission(ctx, f.layout.SchedulerDir(), nil, f.sched, func() time.Time { return f.now }, triggerSweepOptions{}, f.service.delegatedAdmission()); err != nil {
			t.Fatal(err)
		}
		page, err := f.service.queue.Pending(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) > 0 {
			record = page[0]
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	e, err := startintent.Parse(record.Payload)
	if err != nil || e.Target.ConfigGeneration != f.generation || record.State != triggerqueue.Accepted {
		t.Fatal(e, record, err)
	}
	if _, err = f.layout.FindRunDir(strings.TrimPrefix(record.ID, "trigger-")); err == nil {
		t.Fatal("execution preceded queue acceptance")
	}
	writeFileContent(t, f.source, "invalid authored source: [")
	f.setup.OrdinaryCatalog.mu.Lock()
	f.setup.OrdinaryCatalog.generation = "later-applied-generation"
	f.setup.OrdinaryCatalog.mu.Unlock()
	f.drain(t)
	f.sched.Wait()
	f.wg.Wait()
	f.drain(t)
	if err = sweepPendingTriggersWithAdmission(ctx, f.layout.SchedulerDir(), nil, f.sched, func() time.Time { return f.now }, triggerSweepOptions{}, f.service.delegatedAdmission()); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 || !strings.Contains(stdout.String(), "run dispatched via live daemon: "+strings.TrimPrefix(record.ID, "trigger-")) {
			t.Fatal(code, stdout.String(), stderr.String())
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	reader, err := journal.OpenReadOnly(filepath.Join(f.layout.ForGaggle("example").RunsDir(), strings.TrimPrefix(record.ID, "trigger-")))
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
}
