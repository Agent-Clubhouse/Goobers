package retention

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

func TestPruneOrphansReportsThenDeletesOnlyOldMissingJournals(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	layout := instance.NewLayout(t.TempDir())
	runLayout := layout.ForGaggle("example")
	if err := layout.EnsureGaggleRuntime("example"); err != nil {
		t.Fatal(err)
	}
	oldOrphan := filepath.Join(runLayout.RunsDir(), "old-orphan")
	recentOrphan := filepath.Join(runLayout.RunsDir(), "recent-orphan")
	validRun := filepath.Join(runLayout.RunsDir(), "valid-run")
	for _, dir := range []string{oldOrphan, recentOrphan, validRun} {
		if err := os.MkdirAll(filepath.Join(dir, "spans"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".lock"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(validRun, "run.yaml"), []byte("runId: valid-run\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setTreeModTime(t, oldOrphan, now.Add(-48*time.Hour))
	setTreeModTime(t, validRun, now.Add(-48*time.Hour))
	setTreeModTime(t, recentOrphan, now.Add(-time.Hour))

	opts := OrphanOptions{Now: now, MinAge: MinimumOrphanAge}
	results, err := PruneOrphans(layout, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Name != "old-orphan" {
		t.Fatalf("dry-run candidates = %#v, want old-orphan", results)
	}
	for _, dir := range []string{oldOrphan, recentOrphan, validRun} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("dry-run removed %s: %v", dir, err)
		}
	}

	opts.Delete = true
	results, err = PruneOrphans(layout, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Name != "old-orphan" {
		t.Fatalf("deleted candidates = %#v, want old-orphan", results)
	}
	if _, err := os.Stat(oldOrphan); !os.IsNotExist(err) {
		t.Fatalf("old orphan remains: %v", err)
	}
	for _, dir := range []string{recentOrphan, validRun} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("cleanup removed preserved directory %s: %v", dir, err)
		}
	}
}

func TestPruneOrphansUsesNewestContentModification(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	layout := instance.NewLayout(t.TempDir())
	orphan := filepath.Join(layout.RunsDir(), "active-orphan")
	span := filepath.Join(orphan, "spans", "spans.jsonl")
	if err := os.MkdirAll(filepath.Dir(span), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(span, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setTreeModTime(t, orphan, now.Add(-48*time.Hour))
	if err := os.Chtimes(span, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	results, err := PruneOrphans(layout, OrphanOptions{
		Now: now, MinAge: MinimumOrphanAge, Delete: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("recently active orphan selected: %#v", results)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Fatalf("recently active orphan removed: %v", err)
	}
}

func TestPruneOrphansFinishesInterruptedDeletion(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.RunsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(layout.RunsDir(), "interrupted-orphan")
	if err := os.MkdirAll(filepath.Join(orphan, "spans"), 0o755); err != nil {
		t.Fatal(err)
	}
	setTreeModTime(t, orphan, now.Add(-48*time.Hour))
	stagingRoot := orphanStagingRoot(layout.RunsDir())
	if err := os.MkdirAll(stagingRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(stagingRoot, filepath.Base(orphan))
	if err := os.Rename(orphan, staged); err != nil {
		t.Fatal(err)
	}

	results, err := PruneOrphans(layout, OrphanOptions{
		Now: now, MinAge: MinimumOrphanAge, Delete: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].RunDir != orphan {
		t.Fatalf("finished deletions = %#v, want %s", results, orphan)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("staged orphan remains: %v", err)
	}
}

func setTreeModTime(t *testing.T, root string, at time.Time) {
	t.Helper()
	var paths []string
	if err := filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		if err == nil {
			paths = append(paths, path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for index := len(paths) - 1; index >= 0; index-- {
		if err := os.Chtimes(paths[index], at, at); err != nil {
			t.Fatal(err)
		}
	}
}
