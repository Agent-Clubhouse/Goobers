package instance

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// isCompatibilityAlias reports whether path is a legacy-runtime compatibility
// alias, and fails the test if path does not exist.
//
// Tests must not check fs.ModeSymlink for this. On Windows the alias is a
// directory junction, which Go 1.23+ deliberately does not report as a symlink,
// so a ModeSymlink check reads false for a perfectly good alias — and, worse,
// reads false for a missing one too, so the negative assertions pass for the
// wrong reason. isLegacyRuntimeAlias is the platform-aware predicate the
// production code itself uses.
func isCompatibilityAlias(t *testing.T, path string) bool {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	alias, err := isLegacyRuntimeAlias(path, info)
	if err != nil {
		t.Fatalf("inspect compatibility alias %s: %v", path, err)
	}
	return alias
}

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

func TestWorkcopiesDirsIncludesScopedAndLegacyRoots(t *testing.T) {
	layout := NewLayout(t.TempDir())
	for _, dir := range []string{
		layout.WorkcopiesDir(),
		layout.ForGaggle("alpha").WorkcopiesDir(),
		layout.ForGaggle("beta").WorkcopiesDir(),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	got, err := layout.WorkcopiesDirs()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		layout.ForGaggle("alpha").WorkcopiesDir(),
		layout.ForGaggle("beta").WorkcopiesDir(),
		layout.WorkcopiesDir(),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WorkcopiesDirs = %v, want %v", got, want)
	}

	// A scoped layout returns only its own root.
	scopedGot, err := layout.ForGaggle("alpha").WorkcopiesDirs()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{layout.ForGaggle("alpha").WorkcopiesDir()}; !reflect.DeepEqual(scopedGot, want) {
		t.Fatalf("scoped WorkcopiesDirs = %v, want %v", scopedGot, want)
	}
}

func TestWorkcopiesDirsSkipsLegacySymlinkAlias(t *testing.T) {
	layout := NewLayout(t.TempDir())
	scoped := layout.ForGaggle("alpha")
	if err := os.MkdirAll(scoped.WorkcopiesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(scoped.WorkcopiesDir(), layout.WorkcopiesDir()); err != nil {
		t.Fatal(err)
	}

	got, err := layout.WorkcopiesDirs()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{scoped.WorkcopiesDir()}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WorkcopiesDirs with legacy symlink alias = %v, want %v (no double-scan)", got, want)
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

	migration, err := layout.MigrateLegacyRuntimeWithReport([]string{"alpha"})
	if err != nil {
		t.Fatalf("MigrateLegacyRuntime: %v", err)
	}
	if migration.Gaggle != "alpha" || !reflect.DeepEqual(migration.MovedDirs, []string{RunsDirName, WorkcopiesDirName}) {
		t.Fatalf("migration report = %+v", migration)
	}
	if migration.ID == "" {
		t.Fatal("migration report has no durable id")
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
		if !isCompatibilityAlias(t, path) {
			t.Fatalf("legacy path %s is not a compatibility alias", path)
		}
	}
	migration, err = layout.MigrateLegacyRuntimeWithReport([]string{"alpha"})
	if err != nil {
		t.Fatalf("repeat MigrateLegacyRuntime: %v", err)
	}
	if migration.ID == "" || migration.Gaggle != "alpha" || !reflect.DeepEqual(migration.MovedDirs, []string{RunsDirName, WorkcopiesDirName}) {
		t.Fatalf("recovered migration report = %+v", migration)
	}
	if err := layout.CompleteLegacyRuntimeMigration(migration); err != nil {
		t.Fatalf("CompleteLegacyRuntimeMigration: %v", err)
	}
	migration, err = layout.MigrateLegacyRuntimeWithReport([]string{"alpha"})
	if err != nil {
		t.Fatalf("post-completion MigrateLegacyRuntime: %v", err)
	}
	if migration.ID != "" || migration.Gaggle != "" || len(migration.MovedDirs) != 0 {
		t.Fatalf("post-completion migration report = %+v, want empty", migration)
	}
}

func TestMigrateLegacyRuntimeRetainsRetryStateUntilMetadataIsDurable(t *testing.T) {
	layout := NewLayout(t.TempDir())
	legacyRun := filepath.Join(layout.RunsDir(), "run-1", "run.yaml")
	legacyRepo := filepath.Join(layout.WorkcopiesDir(), "repo", "repo.git", "HEAD")
	for _, path := range []string{legacyRun, legacyRepo} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("legacy\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	scopedRoot := layout.ForGaggle("alpha").runtimeRoot()
	syncFailure := errors.New("injected metadata sync failure")
	var attempted []string
	_, err := layout.migrateLegacyRuntimeWithReport([]string{"alpha"}, func(path string) error {
		if _, statErr := os.Stat(layout.legacyRuntimeMigrationPath()); statErr != nil {
			t.Fatalf("retry state missing before metadata sync of %s: %v", path, statErr)
		}
		attempted = append(attempted, path)
		if path == layout.Root {
			return syncFailure
		}
		return nil
	})
	if !errors.Is(err, syncFailure) {
		t.Fatalf("MigrateLegacyRuntimeWithReport error = %v, want %v", err, syncFailure)
	}
	if want := []string{scopedRoot, layout.GagglesDir(), layout.Root}; !reflect.DeepEqual(attempted, want) {
		t.Fatalf("metadata sync attempts = %v, want %v", attempted, want)
	}
	for _, path := range []string{
		filepath.Join(scopedRoot, RunsDirName, "run-1", "run.yaml"),
		filepath.Join(scopedRoot, WorkcopiesDirName, "repo", "repo.git", "HEAD"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("migrated path missing before metadata became durable: %s: %v", path, err)
		}
	}
	for _, path := range []string{layout.RunsDir(), layout.WorkcopiesDir()} {
		if !isCompatibilityAlias(t, path) {
			t.Fatalf("compatibility alias missing before metadata became durable: %s", path)
		}
	}
	pending, exists, err := layout.readLegacyRuntimeMigration()
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("metadata sync failure cleared migration retry state")
	}

	attempted = nil
	recovered, err := layout.migrateLegacyRuntimeWithReport([]string{"alpha"}, func(path string) error {
		if _, statErr := os.Stat(layout.legacyRuntimeMigrationPath()); statErr != nil {
			t.Fatalf("retry state missing during restart sync of %s: %v", path, statErr)
		}
		attempted = append(attempted, path)
		return nil
	})
	if err != nil {
		t.Fatalf("restart MigrateLegacyRuntimeWithReport: %v", err)
	}
	if !reflect.DeepEqual(recovered, pending) {
		t.Fatalf("recovered migration = %+v, want %+v", recovered, pending)
	}
	if want := []string{scopedRoot, layout.GagglesDir(), layout.Root}; !reflect.DeepEqual(attempted, want) {
		t.Fatalf("restart metadata sync attempts = %v, want %v", attempted, want)
	}
	err = layout.completeLegacyRuntimeMigration(recovered, func(path string) error {
		if _, statErr := os.Stat(layout.legacyRuntimeMigrationPath()); statErr != nil {
			t.Fatalf("retry state missing during completion sync of %s: %v", path, statErr)
		}
		if path == layout.Root {
			return syncFailure
		}
		return nil
	})
	if !errors.Is(err, syncFailure) {
		t.Fatalf("CompleteLegacyRuntimeMigration error = %v, want %v", err, syncFailure)
	}
	if _, err := os.Stat(layout.legacyRuntimeMigrationPath()); err != nil {
		t.Fatalf("completion sync failure cleared migration retry state: %v", err)
	}
	if err := layout.CompleteLegacyRuntimeMigration(recovered); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(layout.legacyRuntimeMigrationPath()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("migration retry state after completion: %v", err)
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
		if isCompatibilityAlias(t, legacy) {
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
			if !isCompatibilityAlias(t, alias) {
				t.Fatalf("legacy alias %s was not retained", alias)
			}
		}
		if _, err := os.Stat(legacyRepo); err != nil {
			t.Fatalf("retained workcopies alias is unusable: %v", err)
		}
		// ResolveRuntimeAlias, not EvalSymlinks: the left-hand side is the
		// alias itself, which is a junction on Windows and would otherwise
		// resolve to its own path. The right-hand side is a real directory, so
		// EvalSymlinks is correct there and normalises it the same way.
		target, err := ResolveRuntimeAlias(layout.WorkcopiesDir())
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
