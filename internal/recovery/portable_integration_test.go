//go:build integration

package recovery

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestIntegrationPortableChildTreeRoundTripExcludesHistory(t *testing.T) {
	repo, key, at := childSnapshotFixture(t)
	childSnapshotWrite(t, repo, "credential.txt", "old secret")
	recoveryTestGit(t, repo, "add", "credential.txt")
	recoveryTestGit(t, repo, "commit", "-m", "old credential")
	old := recoveryTestGit(t, repo, "rev-parse", "HEAD:credential.txt")
	policy := SnapshotPolicy{ExcludedPaths: []string{"credential.txt"}}
	childSnapshotWrite(t, repo, "tracked.txt", "parent edits\n")
	before := recoveryTestGit(t, repo, "rev-parse", "HEAD")
	expected := captureChildFixture(t, repo, key, "child", at, policy)
	var bundle bytes.Buffer
	portable, err := WritePortableSnapshot(t.Context(), repo, expected, &bundle, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "input.bundle")
	if err = os.WriteFile(archive, bundle.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	pod := t.TempDir()
	if err = InitializePortableWorkspace(t.Context(), pod, archive, portable, 16<<20); err != nil {
		t.Fatal(err)
	}
	if got := recoveryTestGit(t, pod, "rev-list", "--count", "HEAD"); got != "1" {
		t.Fatal("source history crossed transport", got)
	}
	if err = recoveryGit(t.Context(), pod, &bytes.Buffer{}, "cat-file", "-e", old); err == nil {
		t.Fatal("excluded old credential object crossed transport")
	}
	childSnapshotWrite(t, pod, "tracked.txt", "pod contribution\n")
	childSnapshotWrite(t, pod, "new.bin", "\x00\xff")
	if err = os.Remove(filepath.Join(pod, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	result := captureChildFixture(t, pod, key, "child", at, policy)
	bundle.Reset()
	returned, err := WritePortableSnapshot(t.Context(), pod, result, &bundle, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(archive, bundle.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err = ImportPortableSnapshot(t.Context(), repo, archive, returned, 16<<20); err != nil {
		t.Fatal(err)
	}
	plan, err := PreparePortableReturn(t.Context(), repo, expected, returned, "pod-return", at, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err = ApplyChildApplication(t.Context(), repo, plan); err != nil {
		t.Fatal(err)
	}
	if err = ApplyChildApplication(t.Context(), repo, plan); err != nil {
		t.Fatal("replay", err)
	}
	if got := recoveryTestGit(t, repo, "rev-parse", "HEAD"); got != before {
		t.Fatal("return moved real HEAD")
	}
	if got, err := os.ReadFile(filepath.Join(repo, "credential.txt")); err != nil || string(got) != "old secret" {
		t.Fatal("excluded path changed", err)
	}
	if got, err := os.ReadFile(filepath.Join(repo, "tracked.txt")); err != nil || string(got) != "pod contribution\n" {
		t.Fatal("missing contribution", err)
	}
}
