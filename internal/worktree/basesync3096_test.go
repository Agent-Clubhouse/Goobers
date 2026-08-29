package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const manifestPath = "package.json"

const manifestBase = `{
  "scripts": [
    "lint",
    "test"
  ]
}
`

// manifestWithEntry inserts entry immediately after the "lint" entry, the way
// two independent implementations each adding one script to the same manifest
// list would.
func manifestWithEntry(entry string) string {
	return strings.Replace(manifestBase, `    "lint",`+"\n", `    "lint",`+"\n"+`    "`+entry+`",`+"\n", 1)
}

// TestManager_Create_ResolvesConcurrentManifestLineInsertions is #3096: two
// concurrent implementations each insert a distinct entry into the same
// manifest list, and the sibling lands on base first. The stale branch's
// pre-CI base synchronization must resolve that mechanical conflict and keep
// both entries instead of failing the deterministic CI stage — a conflict no
// implementation repass can fix, because neither branch contains a defect.
func TestManager_Create_ResolvesConcurrentManifestLineInsertions(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	mustWriteFile(t, filepath.Join(repo, manifestPath), manifestBase)
	runTestGit(t, repo, "add", manifestPath)
	runTestGit(t, repo, "commit", "-m", "add manifest")

	m := newTestManager(t)
	const siblingBranch = "goobers/wf/run-sibling"
	const branch = "goobers/wf/run-stale"

	for _, implementation := range []struct {
		branch string
		runID  string
		entry  string
	}{
		{branch: siblingBranch, runID: "run-sibling-implement", entry: "build"},
		{branch: branch, runID: "run-stale-implement", entry: "package"},
	} {
		wt, err := m.Create(ctx, CreateOptions{
			RepoURL: repo, RunID: implementation.runID, BaseRef: "main", Branch: implementation.branch,
		})
		if err != nil {
			t.Fatalf("Create %s: %v", implementation.branch, err)
		}
		mustWriteFile(t, filepath.Join(wt.Path, manifestPath), manifestWithEntry(implementation.entry))
		runTestGit(t, wt.Path, "add", manifestPath)
		runTestGit(t, wt.Path, "commit", "-m", "add "+implementation.entry+" script")
		if err := wt.Remove(ctx, RemoveOptions{}); err != nil {
			t.Fatalf("remove %s worktree: %v", implementation.branch, err)
		}
	}

	// The sibling implementation merges first, so the second branch is now
	// stale against a base that edited the same manifest list.
	mustWriteFile(t, filepath.Join(repo, manifestPath), manifestWithEntry("build"))
	runTestGit(t, repo, "add", manifestPath)
	runTestGit(t, repo, "commit", "-m", "land sibling script")

	synced, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: "run-stale-local-ci", BaseRef: "main", Branch: branch, SyncBase: true,
	})
	if err != nil {
		t.Fatalf("synced Create: %v", err)
	}
	manifest, err := os.ReadFile(filepath.Join(synced.Path, manifestPath))
	if err != nil {
		t.Fatalf("read synced manifest: %v", err)
	}
	for _, entry := range []string{`"build",`, `"package",`, `"lint",`, `"test"`} {
		if !strings.Contains(string(manifest), entry) {
			t.Fatalf("synced manifest = %q, want entry %s retained", manifest, entry)
		}
	}
	if strings.Contains(string(manifest), "<<<<<<<") {
		t.Fatalf("synced manifest = %q, want no conflict markers", manifest)
	}
	if unmerged := strings.TrimSpace(runTestGit(t, synced.Path, "ls-files", "--unmerged")); unmerged != "" {
		t.Fatalf("unmerged paths after sync = %q, want none", unmerged)
	}
	if status := strings.TrimSpace(runTestGit(t, synced.Path, "status", "--porcelain")); status != "" {
		t.Fatalf("worktree status after sync = %q, want a committed merge", status)
	}
	runTestGit(t, synced.Path, "merge-base", "--is-ancestor", "main", "HEAD")
}

// TestManager_Create_KeepsDivergentManifestEditAsConflict guards the safety
// boundary of the resolution above: two implementations editing the SAME
// manifest line disagree substantively, so that conflict must still surface
// under its own machine-readable cause rather than being merged silently.
func TestManager_Create_KeepsDivergentManifestEditAsConflict(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	mustWriteFile(t, filepath.Join(repo, manifestPath), manifestBase)
	runTestGit(t, repo, "add", manifestPath)
	runTestGit(t, repo, "commit", "-m", "add manifest")

	m := newTestManager(t)
	const branch = "goobers/wf/run-divergent"

	first, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: "run-divergent-implement", BaseRef: "main", Branch: branch,
	})
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	mustWriteFile(t, filepath.Join(first.Path, manifestPath), strings.Replace(manifestBase, `"lint",`, `"lint --fix",`, 1))
	runTestGit(t, first.Path, "add", manifestPath)
	runTestGit(t, first.Path, "commit", "-m", "retune lint script")
	if err := first.Remove(ctx, RemoveOptions{}); err != nil {
		t.Fatalf("remove first worktree: %v", err)
	}

	mustWriteFile(t, filepath.Join(repo, manifestPath), strings.Replace(manifestBase, `"lint",`, `"lint --quiet",`, 1))
	runTestGit(t, repo, "add", manifestPath)
	runTestGit(t, repo, "commit", "-m", "land conflicting lint script")

	_, err = m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: "run-divergent-local-ci", BaseRef: "main", Branch: branch, SyncBase: true,
	})
	var conflict *BaseSyncConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("Create error = %v, want BaseSyncConflictError", err)
	}
	if len(conflict.ConflictingFiles) != 1 || conflict.ConflictingFiles[0] != manifestPath {
		t.Fatalf("conflicting files = %v, want [%s]", conflict.ConflictingFiles, manifestPath)
	}
}
