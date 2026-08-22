package instance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSyncGitWorkflowSourceInstallsTrackedCommit is #459's core git-source
// contract: the tracked ref's latest committed definitions replace whatever
// was previously installed in the runtime config directory, and the
// resolved revision names the commit actually installed.
func TestSyncGitWorkflowSourceInstallsTrackedCommit(t *testing.T) {
	repo := newWorkflowSourceSyncTestRepo(t, "manifest-v1\n")
	root := t.TempDir()
	layout := NewLayout(root)
	if err := os.MkdirAll(layout.ConfigDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	// A pre-existing stale file proves the sync replaces the directory
	// wholesale rather than merging into it.
	if err := os.WriteFile(filepath.Join(layout.ConfigDir(), "stale.yaml"), []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	revision, _, err := SyncGitWorkflowSource(context.Background(), root, WorkflowSource{
		Kind: WorkflowSourceKindGit,
		Path: repo,
	}, nil, nil, nil)
	if err != nil {
		t.Fatalf("SyncGitWorkflowSource: %v", err)
	}

	wantRevision := strings.TrimSpace(runWorkflowSourceSyncTestGit(t, repo, "rev-parse", "main"))
	if revision != wantRevision {
		t.Fatalf("revision = %q, want %q", revision, wantRevision)
	}
	assertWorkflowSourceSyncTestFile(t, layout.ConfigDir(), "manifest.yaml", "manifest-v1\n")
	assertWorkflowSourceSyncTestFile(t, layout.ConfigDir(), filepath.Join("gaggles", "example", "gaggle.yaml"), "gaggle-v1\n")
	if _, err := os.Stat(filepath.Join(layout.ConfigDir(), "stale.yaml")); !os.IsNotExist(err) {
		t.Fatalf("stale pre-existing file survived the sync: %v", err)
	}
}

// TestSyncGitWorkflowSourceAdvancesOnNewCommit confirms a second sync after a
// new commit to the tracked ref both updates the installed definitions and
// reports a new revision — the "reconcile now" contract `goobers apply`
// depends on to report Applied/Revision correctly.
func TestSyncGitWorkflowSourceAdvancesOnNewCommit(t *testing.T) {
	repo := newWorkflowSourceSyncTestRepo(t, "manifest-v1\n")
	root := t.TempDir()
	layout := NewLayout(root)
	if err := os.MkdirAll(layout.ConfigDir(), 0o755); err != nil {
		t.Fatal(err)
	}

	source := WorkflowSource{Kind: WorkflowSourceKindGit, Path: repo}
	first, _, err := SyncGitWorkflowSource(context.Background(), root, source, nil, nil, nil)
	if err != nil {
		t.Fatalf("first SyncGitWorkflowSource: %v", err)
	}
	assertWorkflowSourceSyncTestFile(t, layout.ConfigDir(), "manifest.yaml", "manifest-v1\n")

	writeWorkflowSourceSyncTestFile(t, repo, "manifest.yaml", "manifest-v2\n")
	runWorkflowSourceSyncTestGit(t, repo, "add", "manifest.yaml")
	runWorkflowSourceSyncTestGit(t, repo, "commit", "-m", "manifest v2")

	second, _, err := SyncGitWorkflowSource(context.Background(), root, source, nil, nil, nil)
	if err != nil {
		t.Fatalf("second SyncGitWorkflowSource: %v", err)
	}
	if second == first {
		t.Fatalf("revision did not advance after new commit: %s", second)
	}
	assertWorkflowSourceSyncTestFile(t, layout.ConfigDir(), "manifest.yaml", "manifest-v2\n")
}

func TestSyncGitWorkflowSourceIfChangedLeavesCurrentConfigUntouched(t *testing.T) {
	repo := newWorkflowSourceSyncTestRepo(t, "manifest-v1\n")
	root := t.TempDir()
	layout := NewLayout(root)
	if err := os.MkdirAll(layout.ConfigDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	source := WorkflowSource{Kind: WorkflowSourceKindGit, Path: repo}
	revision, changed, _, err := SyncGitWorkflowSourceIfChanged(context.Background(), root, source, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("initial SyncGitWorkflowSourceIfChanged: %v", err)
	}
	if !changed {
		t.Fatal("initial sync reported unchanged")
	}
	sentinel := filepath.Join(layout.ConfigDir(), "runtime-sentinel")
	if err := os.WriteFile(sentinel, []byte("preserve"), 0o644); err != nil {
		t.Fatal(err)
	}

	current, changed, _, err := SyncGitWorkflowSourceIfChanged(context.Background(), root, source, revision, nil, nil, nil)
	if err != nil {
		t.Fatalf("current SyncGitWorkflowSourceIfChanged: %v", err)
	}
	if current != revision || changed {
		t.Fatalf("current sync = (%q, %t), want (%q, false)", current, changed, revision)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("unchanged sync replaced runtime config: %v", err)
	}
}

// TestSyncGitWorkflowSourceRejectsUnsupportedKind pins the guard at this
// seam: a local-dir workflowSource has nothing to pull, so syncing it is a
// caller bug, not a silent no-op.
func TestSyncGitWorkflowSourceRejectsUnsupportedKind(t *testing.T) {
	root := t.TempDir()
	_, _, err := SyncGitWorkflowSource(context.Background(), root, WorkflowSource{
		Kind: WorkflowSourceKindLocalDir,
		Path: t.TempDir(),
	}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "is not") || !strings.Contains(err.Error(), WorkflowSourceKindGit) {
		t.Fatalf("err = %v, want kind-mismatch rejection", err)
	}
}

func newWorkflowSourceSyncTestRepo(t *testing.T, manifestContent string) string {
	t.Helper()
	repo := t.TempDir()
	runWorkflowSourceSyncTestGit(t, repo, "init", "-b", "main")
	runWorkflowSourceSyncTestGit(t, repo, "config", "user.email", "test@example.com")
	runWorkflowSourceSyncTestGit(t, repo, "config", "user.name", "Test")
	writeWorkflowSourceSyncTestFile(t, repo, "manifest.yaml", manifestContent)
	writeWorkflowSourceSyncTestFile(t, repo, filepath.Join("gaggles", "example", "gaggle.yaml"), "gaggle-v1\n")
	runWorkflowSourceSyncTestGit(t, repo, "add", ".")
	runWorkflowSourceSyncTestGit(t, repo, "commit", "-m", "initial")
	return repo
}

func writeWorkflowSourceSyncTestFile(t *testing.T, root, relPath, content string) {
	t.Helper()
	full := filepath.Join(root, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertWorkflowSourceSyncTestFile(t *testing.T, root, relPath, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(root, relPath))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", relPath, got, want)
	}
}

func runWorkflowSourceSyncTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return runGitSourceTest(t, dir, args...)
}
