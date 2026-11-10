package localscheduler

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestReloadDoesNotCommitGenerationBeforePersistence(t *testing.T) {
	scheduler, dir := newTestScheduler(t, []WorkflowEntry{{Workflow: "old"}})
	// Force schedule-demand persistence to fail after trigger state is written.
	path := filepath.Join(dir, scheduleDemandStateFileName)
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}

	if err := scheduler.Reload([]WorkflowEntry{{Workflow: "new"}}, nil, time.Now(), "old", "new", "sha256:new"); err == nil {
		t.Fatal("reload ignored persistence failure")
	}
	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == journal.EventConfigReloaded {
			t.Fatal("failed reload committed recovery authority")
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Reload([]WorkflowEntry{{Workflow: "new"}}, nil, time.Now(), "old", "new", "sha256:new"); err != nil {
		t.Fatal(err)
	}
	events, err = journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == journal.EventConfigReloaded && event.Runner["configGeneration"] == "sha256:new" {
			return
		}
	}
	t.Fatal("successful reload omitted retained generation")
}

func TestReloadRecordsWorkflowSourceProvenance(t *testing.T) {
	scheduler, dir := newTestScheduler(t, []WorkflowEntry{{Workflow: "old"}})
	source := &journal.WorkflowSourceProvenance{
		URL:    "https://github.com/acme/goobers-config.git",
		Dir:    "macmini-octo",
		Commit: "0123456789abcdef0123456789abcdef01234567",
	}
	if err := scheduler.ReloadWithProvenance([]WorkflowEntry{{Workflow: "new"}}, nil, time.Now(), "old", "new", "sha256:new", source); err != nil {
		t.Fatal(err)
	}
	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != journal.EventConfigReloaded {
			continue
		}
		fields, ok := event.Runner["workflowSource"].(map[string]any)
		if !ok || fields["url"] != source.URL || fields["dir"] != source.Dir || fields["commit"] != source.Commit {
			t.Fatalf("workflow source journal fields = %#v", event.Runner["workflowSource"])
		}
		return
	}
	t.Fatal("successful reload omitted config-reloaded event")
}
