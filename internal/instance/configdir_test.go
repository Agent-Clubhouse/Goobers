package instance

import (
	"errors"
	"testing"
)

const (
	validConfigDir = "../../config-examples"
	badConfigDir   = "../../api/validate/testdata/config-bad"
)

func TestLoadConfigDirValid(t *testing.T) {
	set, report, err := LoadConfigDir(validConfigDir)
	if err != nil {
		t.Fatalf("LoadConfigDir: %v (report: %+v)", err, report)
	}
	if set.Manifest == nil {
		t.Fatal("expected a Manifest")
	}
	if len(set.Gaggles) != 1 || set.Gaggles[0].Name != "acme-web" {
		t.Fatalf("unexpected gaggles: %+v", set.Gaggles)
	}
	// config-examples ships five goobers (coder, curator, implementer,
	// nominator — #26, reviewer) and five workflows (default-implement,
	// backlog-curation — #25, implementation — #27, work-nomination — #26,
	// merge-review — #568);
	// check membership, not order.
	gotGoobers := map[string]bool{}
	for _, g := range set.Goobers {
		gotGoobers[g.Name] = true
	}
	wantGoobers := []string{"coder", "curator", "implementer", "nominator", "reviewer"}
	if len(set.Goobers) != len(wantGoobers) {
		t.Fatalf("unexpected goobers: %+v", set.Goobers)
	}
	for _, name := range wantGoobers {
		if !gotGoobers[name] {
			t.Fatalf("missing goober %q; got: %+v", name, set.Goobers)
		}
	}
	gotWorkflows := map[string]bool{}
	for _, w := range set.Workflows {
		gotWorkflows[w.Name] = true
	}
	wantWorkflows := []string{"default-implement", "backlog-curation", "implementation", "work-nomination", "merge-review"}
	if len(set.Workflows) != len(wantWorkflows) {
		t.Fatalf("unexpected workflows: %+v", set.Workflows)
	}
	for _, name := range wantWorkflows {
		if !gotWorkflows[name] {
			t.Fatalf("missing workflow %q; got: %+v", name, set.Workflows)
		}
	}
}

func TestLoadConfigDirInvalid(t *testing.T) {
	set, report, err := LoadConfigDir(badConfigDir)
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig, got %v", err)
	}
	if set != nil {
		t.Fatalf("expected a nil ConfigSet on invalid config, got %+v", set)
	}
	if report == nil || !report.HasErrors() {
		t.Fatalf("expected a report with errors, got %+v", report)
	}
}

func TestLoadConfigDirMissingDir(t *testing.T) {
	_, _, err := LoadConfigDir("../../does/not/exist")
	if err == nil {
		t.Fatal("expected an error for a missing config directory")
	}
}
