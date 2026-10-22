package main

import (
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func TestReleaseLegacyEngineRunReleasesSlotAndWarns(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	log, _, err := journal.OpenInstanceLog(filepath.Join(layout.Root, "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	identity := journal.RunIdentity{RunID: "legacy-engine-run", Gaggle: "example", Workflow: "implement", Driver: journal.DriverEngine}
	var released string
	releaseLegacyEngineRun(log, identity, func(runID, workflow string) {
		released = runID + "/" + workflow
	})
	if released != "legacy-engine-run/implement" {
		t.Fatalf("released = %q", released)
	}
	events, err := journal.ReadInstanceLog(log.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Runner["reason"] != "engine_moved_to_goobers_cloud" {
		t.Fatalf("recovery annotation = %+v", events)
	}
}
