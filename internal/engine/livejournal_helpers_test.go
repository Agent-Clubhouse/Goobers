package engine

import (
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

func newLiveWriter(t *testing.T) (*livejournal.Writer, string) {
	t.Helper()
	runsDir := filepath.Join(t.TempDir(), "runs")
	writer, err := livejournal.NewWriter(func(gaggle string) (string, bool) {
		return runsDir, gaggle == "web"
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	return writer, runsDir
}

func liveEvents(t *testing.T, runsDir, runID string) []journal.Event {
	t.Helper()
	reader, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// writeTestJournal persists a workflow's recorded operations through the retained
// live writer. This fixture adapter replaces the removed host repair projector.
func writeTestJournal(t *testing.T, runsDir string, proj JournalProjection) (string, error) {
	t.Helper()
	writer, err := livejournal.NewWriter(func(string) (string, bool) { return runsDir, true })
	if err != nil {
		return "", err
	}
	defer writer.Close()
	rec := &runJournal{proj: proj}
	rec.assignEmitKeys()
	req := livejournal.EmitRequest{RunID: proj.Identity.RunID, Gaggle: proj.Identity.Gaggle,
		Open: &livejournal.OpenHeader{Identity: proj.Identity, Graph: proj.Graph, Definition: proj.Definition, Item: proj.Item, GateGooberCapabilities: proj.GateGooberCapabilities}}
	for _, op := range rec.proj.Ops {
		req.Ops = append(req.Ops, liveOpFrom(op))
	}
	if _, err := writer.Emit(t.Context(), req); err != nil {
		return "", err
	}
	return filepath.Join(runsDir, proj.Identity.RunID), nil
}

func diffTestJournal(t *testing.T, events []journal.Event, proj JournalProjection) (string, error) {
	t.Helper()
	dir, err := writeTestJournal(t, t.TempDir(), proj)
	if err != nil {
		return "", err
	}
	if err := diffConformanceViews(events, readJournalEvents(t, dir)); err != nil {
		return err.Error(), nil
	}
	return "", nil
}
