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

func TestGateWaitStatusUsesDurableProjectionWithoutJournalReads(t *testing.T) {
	ctx := context.Background()
	store, err := readmodel.Open(filepath.Join(t.TempDir(), readmodel.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := NewLocal(LocalSources{Layout: instance.NewLayout(t.TempDir()), Definitions: testDefinitions(), ReadModel: store}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	identity := journal.RunIdentity{RunID: "gate-wait-run", Gaggle: "goobers", Workflow: "implementation", StartedAt: now}
	var projection readmodel.Projection
	for i, step := range []struct {
		kind    journal.EventType
		waiting bool
	}{
		{journal.EventRunStarted, false}, {journal.EventGatePaused, true},
		{journal.EventRunnerAnnotation, true}, {journal.EventGateStarted, false},
	} {
		event := journal.Event{Schema: journal.EventSchema, Seq: uint64(i + 1), Time: now.Add(time.Duration(i) * time.Second), Type: step.kind, Gate: "approval"}
		projection = readmodel.ProjectRun(identity, projection, []journal.Event{event})
		if err := store.UpsertRun(ctx, projection); err != nil {
			t.Fatal(err)
		}
		readprobe.Enable()
		before := readprobe.Take()
		runs, err := service.ListStatusRuns(ctx, StatusRunOptions{Gaggle: "goobers", Limit: 1})
		work := readprobe.Take().Sub(before)
		readprobe.Disable()
		if err != nil || len(runs) != 1 || runs[0].WaitingForGate != step.waiting || work.JournalOpens != 0 {
			t.Fatalf("kind=%s runs=%+v err=%v journal opens=%d", step.kind, runs, err, work.JournalOpens)
		}
	}
}
