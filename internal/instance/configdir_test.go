package instance

import (
	"errors"
	"os"
	"path/filepath"
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
	// nominator — #26, reviewer) and six workflows (default-implement,
	// backlog-curation — #25, implementation — #27, work-nomination — #26,
	// merge-review — #568, todo-check — #577);
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
	wantWorkflows := []string{"default-implement", "backlog-curation", "implementation", "work-nomination", "merge-review", "todo-check"}
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

func TestLoadConfigDirIgnoresAssetDefinitions(t *testing.T) {
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS(validConfigDir)); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "gaggles", "acme-web", "goobers", "coder", "goober.yaml")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	asset := filepath.Join(filepath.Dir(source), "assets", "duplicate.yaml")
	if err := os.MkdirAll(filepath.Dir(asset), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(asset, data, 0o644); err != nil {
		t.Fatal(err)
	}
	set, report, err := LoadConfigDir(root)
	if err != nil {
		t.Fatalf("LoadConfigDir: %v (report: %+v)", err, report)
	}
	if len(set.Goobers) != 5 {
		t.Fatalf("asset definition leaked into config set: got %d goobers", len(set.Goobers))
	}
}

func TestLoadConfigDirMissingDir(t *testing.T) {
	_, _, err := LoadConfigDir("../../does/not/exist")
	if err == nil {
		t.Fatal("expected an error for a missing config directory")
	}
}
