package instance

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestGaggleRuntimeLayout(t *testing.T) {
	root := t.TempDir()
	layout := NewLayout(root)
	alpha := layout.ForGaggle("alpha")
	beta := layout.ForGaggle("beta")

	if got, want := alpha.RunsDir(), filepath.Join(root, "gaggles", "alpha", "runs"); got != want {
		t.Fatalf("alpha RunsDir = %q, want %q", got, want)
	}
	if got, want := beta.WorkcopiesDir(), filepath.Join(root, "gaggles", "beta", "workcopies"); got != want {
		t.Fatalf("beta WorkcopiesDir = %q, want %q", got, want)
	}
	if alpha.SchedulerDir() != beta.SchedulerDir() || alpha.TelemetryDB() != beta.TelemetryDB() {
		t.Fatal("scheduler and telemetry paths must remain instance-wide")
	}
}

func TestRunDirsAndFindRunDirIncludeScopedAndLegacyRoots(t *testing.T) {
	layout := NewLayout(t.TempDir())
	for _, dir := range []string{
		filepath.Join(layout.RunsDir(), "legacy-run"),
		filepath.Join(layout.ForGaggle("alpha").RunsDir(), "alpha-run"),
		filepath.Join(layout.ForGaggle("beta").RunsDir(), "beta-run"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	got, err := layout.RunDirs()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		layout.ForGaggle("alpha").RunsDir(),
		layout.ForGaggle("beta").RunsDir(),
		layout.RunsDir(),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RunDirs = %v, want %v", got, want)
	}
	if got, err := layout.FindRunDir("beta-run"); err != nil || got != filepath.Join(layout.ForGaggle("beta").RunsDir(), "beta-run") {
		t.Fatalf("FindRunDir(beta-run) = %q, %v", got, err)
	}
	if _, err := layout.FindRunDir("missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("FindRunDir(missing) error = %v, want fs.ErrNotExist", err)
	}
}

func TestMigrateLegacyRuntimeToSingleGaggle(t *testing.T) {
	layout := NewLayout(t.TempDir())
	legacyRun := filepath.Join(layout.RunsDir(), "run-1", "run.yaml")
	legacyRepo := filepath.Join(layout.WorkcopiesDir(), "repo", "repo.git", "HEAD")
	for path, content := range map[string]string{
		legacyRun:  "runId: run-1\n",
		legacyRepo: "ref: refs/heads/main\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := layout.MigrateLegacyRuntime([]string{"alpha"}); err != nil {
		t.Fatalf("MigrateLegacyRuntime: %v", err)
	}
	scoped := layout.ForGaggle("alpha")
	for _, path := range []string{
		filepath.Join(scoped.RunsDir(), "run-1", "run.yaml"),
		filepath.Join(scoped.WorkcopiesDir(), "repo", "repo.git", "HEAD"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("migrated path %s: %v", path, err)
		}
	}
	for _, path := range []string{layout.RunsDir(), layout.WorkcopiesDir()} {
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("legacy path %s is not a compatibility symlink: %v", path, err)
		}
	}
}

func TestMigrateLegacyRuntimeRejectsAmbiguousPopulatedRoot(t *testing.T) {
	layout := NewLayout(t.TempDir())
	if err := os.MkdirAll(filepath.Join(layout.RunsDir(), "run-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := layout.MigrateLegacyRuntime([]string{"alpha", "beta"})
	if err == nil {
		t.Fatal("expected populated legacy root with multiple gaggles to fail")
	}
	if _, statErr := os.Stat(filepath.Join(layout.RunsDir(), "run-1")); statErr != nil {
		t.Fatalf("failed migration changed legacy data: %v", statErr)
	}
}
