package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readservice"
)

// liveEscalationReads builds the daemon-shaped reader: a readservice.Local
// whose list path is read.db, with every run directory already projected.
func liveEscalationReads(t *testing.T, layout instance.Layout) *readservice.Local {
	t.Helper()
	store, err := readmodel.Open(layout.ReadDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	dirs, err := layout.RunDirs()
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				if err := store.ProjectRunDir(context.Background(), filepath.Join(dir, entry.Name())); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	reads, err := readservice.NewLocal(readservice.LocalSources{Layout: layout, Definitions: &instance.ConfigSet{Manifest: &apiv1.Manifest{}}, ReadModel: store}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	return reads
}

// spyEscalationReads records the list options and delegates to the live reader.
type spyEscalationReads struct {
	*readservice.Local
	mu    sync.Mutex
	lists []readservice.RunListOptions
}

func (s *spyEscalationReads) ListRuns(ctx context.Context, o readservice.RunListOptions) (readservice.RunList, error) {
	s.mu.Lock()
	s.lists = append(s.lists, o)
	s.mu.Unlock()
	return s.Local.ListRuns(ctx, o)
}

func seedLiveEscalationFixture(t *testing.T) instance.Layout {
	t.Helper()
	layout := crossRunTestLayout(t)
	askingRun, err := journal.Create(layout.ForGaggle(crossRunTestGaggle).RunsDir(), journal.RunIdentity{
		RunID: "asking-run", Workflow: "implementation", WorkflowVersion: 1, Gaggle: crossRunTestGaggle,
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := askingRun.Close(); err != nil {
		t.Fatal(err)
	}
	seedEscalatedRun(t, layout, crossRunTestGaggle, "escalated-a", "501")
	seedEscalatedRun(t, layout, crossRunTestGaggle, "escalated-b", "502")
	seedEscalatedRun(t, layout, "other-gaggle", "escalated-theirs", "999")
	seedBranchOwningRun(t, layout, crossRunTestGaggle, "completed-run", "implementation", "goobers/x")
	return layout
}

// The daemon route must list escalated runs through the live read model, scoped
// to the gaggle by phase, and never through the offline journal scan.
func TestDaemonEscalationCandidatesUsesLiveReadModel(t *testing.T) {
	layout := seedLiveEscalationFixture(t)
	spy := &spyEscalationReads{Local: liveEscalationReads(t, layout)}

	// Corrupt a NON-escalated run's journal after projection. The offline scan
	// opens every run journal and would fail on it; the read-model list never
	// opens it.
	if err := os.WriteFile(filepath.Join(layout.ForGaggle(crossRunTestGaggle).RunsDir(), "completed-run", "run.yaml"), []byte("::not yaml::"), 0o644); err != nil {
		t.Fatal(err)
	}

	service := newDaemonRunJournalService(layout, nil)
	service.reads = spy
	ask := journalclient.EscalationCandidatesRequest{RunID: "asking-run", Gaggle: crossRunTestGaggle}
	response, err := service.EscalationCandidates(context.Background(), ask)
	if err != nil {
		t.Fatalf("escalation candidates: %v", err)
	}
	if len(response.Candidates) != 2 || response.Candidates[0].ParentID != "501" || response.Candidates[1].ParentID != "502" {
		t.Fatalf("candidates = %+v, want parents 501 then 502", response.Candidates)
	}
	if len(spy.lists) == 0 {
		t.Fatal("the live read model was never asked to list runs")
	}
	for _, o := range spy.lists {
		if o.Phase != journal.PhaseEscalated || o.Gaggle != crossRunTestGaggle {
			t.Fatalf("list options = %+v, want phase escalated scoped to %s", o, crossRunTestGaggle)
		}
	}

	// Control: the offline scan really does trip over that journal.
	if _, err := journalclient.NewFileCrossRun(layout).EscalationCandidates(context.Background(), ask); err == nil {
		t.Fatal("control failed: the offline scan did not touch the corrupted run")
	}
}

// The live path and the file-backed path must select exactly the same
// candidates, in the same order.
func TestDaemonEscalationCandidatesParityWithFileCrossRun(t *testing.T) {
	layout := seedLiveEscalationFixture(t)
	service := newDaemonRunJournalService(layout, nil)
	service.reads = liveEscalationReads(t, layout)
	ask := journalclient.EscalationCandidatesRequest{RunID: "asking-run", Gaggle: crossRunTestGaggle}

	live, err := service.EscalationCandidates(context.Background(), ask)
	if err != nil {
		t.Fatal(err)
	}
	file, err := journalclient.NewFileCrossRun(layout).EscalationCandidates(context.Background(), ask)
	if err != nil {
		t.Fatal(err)
	}
	// Same instants; the read model hands back UTC, the journal its local zone.
	for i := range file {
		file[i].StartedAt = file[i].StartedAt.UTC()
	}
	for i := range live.Candidates {
		live.Candidates[i].StartedAt = live.Candidates[i].StartedAt.UTC()
	}
	if len(file) == 0 || !reflect.DeepEqual(live.Candidates, file) {
		t.Fatalf("live = %+v\nfile = %+v", live.Candidates, file)
	}
}

func TestDaemonEscalationCandidatesRefusesWithoutLiveReads(t *testing.T) {
	layout := seedLiveEscalationFixture(t)
	service := newDaemonRunJournalService(layout, nil)
	if _, err := service.EscalationCandidates(context.Background(), journalclient.EscalationCandidatesRequest{RunID: "asking-run", Gaggle: crossRunTestGaggle}); err == nil {
		t.Fatal("daemon served escalation candidates without a live read model (silent offline fallback)")
	}
}
