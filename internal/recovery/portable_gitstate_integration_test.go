//go:build integration

package recovery

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationPortableGitStatePreservesIndependentTrees(t *testing.T) {
	testdep.Require(t, "git")
	repo, key, at := childSnapshotFixture(t)
	childSnapshotWrite(t, repo, "credential.txt", "private history")
	childSnapshotWrite(t, repo, ".gitattributes", "*.txt text eol=crlf\n")
	childSnapshotWrite(t, repo, "stable.txt", "unchanged bytes\n")
	recoveryTestGit(t, repo, "add", "credential.txt", ".gitattributes", "stable.txt")
	recoveryTestGit(t, repo, "commit", "-m", "credential")
	secret := recoveryTestGit(t, repo, "rev-parse", "HEAD:credential.txt")
	childSnapshotWrite(t, repo, "tracked.txt", "staged\n")
	childSnapshotWrite(t, repo, "staged.txt", "staged addition\n")
	recoveryTestGit(t, repo, "add", "tracked.txt", "staged.txt")
	recoveryTestGit(t, repo, "rm", "deleted.txt")
	childSnapshotWrite(t, repo, "tracked.txt", "working\r\n")
	childSnapshotWrite(t, repo, "untracked.bin", "\x00\xff")
	policy := SnapshotPolicy{ExcludedPaths: []string{"credential.txt"}}
	snapshot := captureChildFixture(t, repo, key, "parent", at, policy)
	headBefore := recoveryTestGit(t, repo, "rev-parse", "HEAD")
	indexPath := filepath.Join(repo, ".git", "index")
	indexBefore, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	var headBytes, indexBytes, workBytes bytes.Buffer
	head, err := WritePortableGitState(t.Context(), repo, snapshot, false, &headBytes, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	index, err := WritePortableGitState(t.Context(), repo, snapshot, true, &indexBytes, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	working, err := WritePortableSnapshot(t.Context(), repo, snapshot, &workBytes, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	pod := t.TempDir()
	archive := filepath.Join(t.TempDir(), "state.bundle")
	for i, entry := range []struct {
		state PortableSnapshot
		data  []byte
	}{{head, headBytes.Bytes()}, {index, indexBytes.Bytes()}, {working, workBytes.Bytes()}} {
		if err := os.WriteFile(archive, entry.data, 0600); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			err = InitializePortableWorkspace(t.Context(), pod, archive, entry.state, 16<<20)
		} else {
			err = ImportPortableSnapshot(t.Context(), pod, archive, entry.state, 16<<20)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := MaterializePortableGitState(t.Context(), pod, head, index, working); err != nil {
		t.Fatal(err)
	}
	for ref, want := range map[string]string{"HEAD:tracked.txt": "base", ":tracked.txt": "staged", ":staged.txt": "staged addition", "HEAD:deleted.txt": "delete me"} {
		if got := recoveryTestGit(t, pod, "show", ref); got != want {
			t.Fatalf("%s = %q, want %q", ref, got, want)
		}
	}
	for name, want := range map[string]string{"tracked.txt": "working\r\n", "untracked.bin": "\x00\xff", "stable.txt": "unchanged bytes\n"} {
		got, err := os.ReadFile(filepath.Join(pod, name))
		if err != nil || string(got) != want {
			t.Fatalf("working file %s = %q: %v", name, got, err)
		}
	}
	if got := recoveryTestGit(t, pod, "ls-files", "deleted.txt", "untracked.bin"); got != "" {
		t.Fatalf("index incorrectly contains deleted/untracked files: %s", got)
	}
	if _, err := os.Stat(filepath.Join(pod, "deleted.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("working deletion lost: %v", err)
	}
	if err := recoveryGit(t.Context(), pod, &bytes.Buffer{}, "cat-file", "-e", secret); err == nil {
		t.Fatal("excluded history leaked into portable state")
	}
	if got := recoveryTestGit(t, pod, "rev-list", "--count", "HEAD"); got != "1" {
		t.Fatal("source ancestry crossed transport")
	}
	indexAfter, err := os.ReadFile(indexPath)
	if err != nil || !bytes.Equal(indexBefore, indexAfter) || recoveryTestGit(t, repo, "rev-parse", "HEAD") != headBefore {
		t.Fatal("capture modified source Git state", err)
	}
	// A real worker commit must consume the original staged tree, not the
	// incoming uncommitted working edits presented by tree-only transport.
	recoveryTestGit(t, pod, "commit", "-m", "worker commits staged work")
	if got := recoveryTestGit(t, pod, "show", "HEAD:tracked.txt"); got != "staged" {
		t.Fatal("worker accidentally committed unstaged input", got)
	}
	if err := MaterializePortableGitState(t.Context(), pod, head, index, working); err == nil {
		t.Fatal("materialization reset an already advanced worker")
	}
	recoveryTestGit(t, repo, "add", "tracked.txt")
	if _, err := WritePortableGitState(t.Context(), repo, snapshot, true, &bytes.Buffer{}, 16<<20); !errors.Is(err, ErrWorkspaceChanged) {
		t.Fatalf("stale index capture accepted: %v", err)
	}
}
