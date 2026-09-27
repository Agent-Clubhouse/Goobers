package readservice

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readprobe"
)

func TestRetryBackoffStatusUsesDurableProjectionAndClears(t *testing.T) {
	ctx := context.Background()
	store, err := readmodel.Open(filepath.Join(t.TempDir(), readmodel.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	service, err := NewLocal(LocalSources{Layout: instance.NewLayout(t.TempDir()), Definitions: testDefinitions(), ReadModel: store}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	identity := journal.RunIdentity{RunID: "retry-status", Gaggle: "goobers", Workflow: "implementation", StartedAt: now}
	wait := journal.RetryBackoffEvent("implement", 1, "engine", journal.AttemptInfra, now, now.Add(time.Minute))
	wait.Schema, wait.Time, wait.Seq = journal.EventSchema, now, 1
	start := journal.Event{Schema: journal.EventSchema, Type: journal.EventStageStarted, Stage: "implement", Attempt: 2, Time: now.Add(time.Minute), Seq: 2}
	var projection readmodel.Projection
	for i, event := range []journal.Event{wait, start} {
		projection = readmodel.ProjectRun(identity, projection, []journal.Event{event})
		if err := store.UpsertRun(ctx, projection); err != nil {
			t.Fatal(err)
		}
		readprobe.Enable()
		before := readprobe.Take()
		runs, err := service.ListStatusRuns(ctx, StatusRunOptions{Gaggle: "goobers", Limit: 1})
		work := readprobe.Take().Sub(before)
		readprobe.Disable()
		if err != nil || len(runs) != 1 {
			t.Fatalf("runs=%+v err=%v", runs, err)
		}
		if len(runs[0].RetryBackoff.Waits) != 1-i || work.JournalOpens != 0 {
			t.Fatalf("projection=%+v opens=%d", runs[0].RetryBackoff, work.JournalOpens)
		}
		if i == 0 && !runs[0].RetryBackoff.Waits[0].Deadline.Equal(now.Add(time.Minute)) {
			t.Fatal("deadline lost through SQLite")
		}
	}
}
