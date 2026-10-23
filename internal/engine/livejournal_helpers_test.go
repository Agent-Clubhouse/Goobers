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
