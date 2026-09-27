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

func TestRequiredMCPStatusProjectionRecoversWithoutJournalReads(t *testing.T) {
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
	identity := journal.RunIdentity{RunID: "readiness-run", Gaggle: "goobers", Workflow: "implementation", StartedAt: now}
	var projection readmodel.Projection
	for i, category := range []string{"required_tool_unavailable", "ready"} {
		event := journal.Event{Schema: journal.EventSchema, Seq: uint64(i + 1), Time: now.Add(time.Duration(i) * time.Second), Type: journal.EventRunnerAnnotation, Stage: "implement", Runner: map[string]any{"kind": "required-mcp-readiness", "schemaVersion": 1, "adapter": "copilot-cli", "server": "goobers-io", "category": category, "connection": "ready", "inventory": "ready", "authorization": "ready"}}
		projection = readmodel.ProjectRun(identity, projection, []journal.Event{event})
		if err := store.UpsertRun(ctx, projection); err != nil {
			t.Fatal(err)
		}
		readprobe.Enable()
		before := readprobe.Take()
		runs, err := service.ListStatusRuns(ctx, StatusRunOptions{Gaggle: "goobers", Limit: 1})
		work := readprobe.Take().Sub(before)
		readprobe.Disable()
		if err != nil || len(runs) != 1 || runs[0].RequiredMCP == nil {
			t.Fatalf("runs=%+v err=%v", runs, err)
		}
		condition := runs[0].RequiredMCP.Conditions[0]
		if condition.Active != (i == 0) || condition.Adapter != "copilot-cli" || condition.Stage != "implement" || !condition.ObservedAt.Equal(event.Time) || work.JournalOpens != 0 {
			t.Fatalf("condition=%+v journalOpens=%d", condition, work.JournalOpens)
		}
	}
}
