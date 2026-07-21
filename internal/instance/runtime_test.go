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

func TestMigrateLegacyRuntimePreservesPopulatedRootForMultipleGaggles(t *testing.T) {
	layout := NewLayout(t.TempDir())
	legacyPaths := []string{
		filepath.Join(layout.RunsDir(), "run-1", "run.yaml"),
		filepath.Join(layout.WorkcopiesDir(), "repo", "repo.git", "HEAD"),
	}
	for _, path := range legacyPaths {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("legacy\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := layout.MigrateLegacyRuntime([]string{"alpha", "beta"}); err != nil {
		t.Fatalf("MigrateLegacyRuntime: %v", err)
	}
	for _, path := range legacyPaths {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("legacy path %s was not preserved: %v", path, err)
		}
	}
	for _, gaggle := range []string{"alpha", "beta"} {
		for _, dir := range []string{
			layout.ForGaggle(gaggle).RunsDir(),
			layout.ForGaggle(gaggle).WorkcopiesDir(),
		} {
			if info, err := os.Stat(dir); err != nil || !info.IsDir() {
				t.Fatalf("scoped runtime directory %s was not created: %v", dir, err)
			}
		}
	}
}

func TestMigrateLegacyRuntimePreservesAmbiguousRootAfterReducingToOneGaggle(t *testing.T) {
	layout := NewLayout(t.TempDir())
	legacyRun := filepath.Join(layout.RunsDir(), "legacy-run", "run.yaml")
	legacyWorkcopy := filepath.Join(layout.WorkcopiesDir(), "legacy-repo", "repo.git", "HEAD")
	for path, content := range map[string]string{
		legacyRun:      "gaggle: alpha\n",
		legacyWorkcopy: "ref: refs/heads/main\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := layout.MigrateLegacyRuntime([]string{"alpha", "beta"}); err != nil {
		t.Fatalf("multi-gaggle migration: %v", err)
	}
	scopedRun := filepath.Join(layout.ForGaggle("beta").RunsDir(), "new-run", "run.yaml")
	if err := os.MkdirAll(filepath.Dir(scopedRun), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scopedRun, []byte("gaggle: beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := layout.MigrateLegacyRuntime([]string{"beta"}); err != nil {
		t.Fatalf("transition to populated sole gaggle: %v", err)
	}
	for _, path := range []string{legacyRun, legacyWorkcopy, scopedRun} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("retained runtime path %s: %v", path, err)
		}
	}
	if err := layout.MigrateLegacyRuntime([]string{"gamma"}); err != nil {
		t.Fatalf("transition to new sole gaggle: %v", err)
	}
	for _, legacy := range []string{layout.RunsDir(), layout.WorkcopiesDir()} {
		info, err := os.Lstat(legacy)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("ambiguous legacy runtime %s became a single-gaggle alias", legacy)
		}
	}
	if _, err := os.Stat(filepath.Join(layout.ForGaggle("gamma").RunsDir(), "legacy-run")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("legacy run was assigned to gamma: %v", err)
	}
}

func TestMigrateLegacyRuntimeRetainsGeneratedAliases(t *testing.T) {
	layout := NewLayout(t.TempDir())
	legacyRepo := filepath.Join(layout.WorkcopiesDir(), "repo", "repo.git", "HEAD")
	if err := os.MkdirAll(filepath.Dir(legacyRepo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyRepo, []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := layout.MigrateLegacyRuntime([]string{"alpha"}); err != nil {
		t.Fatal(err)
	}

	for _, gaggles := range [][]string{{"beta"}, {"alpha", "beta"}} {
		if err := layout.MigrateLegacyRuntime(gaggles); err != nil {
			t.Fatalf("MigrateLegacyRuntime(%v): %v", gaggles, err)
		}
		for _, alias := range []string{layout.RunsDir(), layout.WorkcopiesDir()} {
			info, err := os.Lstat(alias)
			if err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("legacy alias %s was not retained: %v", alias, err)
			}
		}
		if _, err := os.Stat(legacyRepo); err != nil {
			t.Fatalf("retained workcopies alias is unusable: %v", err)
		}
		target, err := filepath.EvalSymlinks(layout.WorkcopiesDir())
		if err != nil {
			t.Fatal(err)
		}
		want, err := filepath.EvalSymlinks(layout.ForGaggle("alpha").WorkcopiesDir())
		if err != nil {
			t.Fatal(err)
		}
		if target != want {
			t.Fatalf("workcopies alias target = %q, want retained owner %q", target, want)
		}
	}
}
