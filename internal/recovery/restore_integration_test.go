//go:build integration

package recovery

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationRestoreFullPatchOntoCurrentMain(t *testing.T) {
	testdep.Require(t, "git")
	repository := t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	writeRestoreFixture(t, repository, "shared.txt", "base\n")
	recoveryTestGit(t, repository, "add", ".")
	recoveryTestGit(t, repository, "commit", "-m", "base")
	record := storageTestRecord()
	record.BaseSHA = recoveryTestGit(t, repository, "rev-parse", "HEAD")
	recoveryTestGit(t, repository, "checkout", "-b", "implementation")
	writeRestoreFixture(t, repository, "shared.txt", "ours\n")
	writeRestoreFixture(t, repository, "reviewed.txt", "reviewed work")
	recoveryTestGit(t, repository, "add", ".")
	recoveryTestGit(t, repository, "commit", "-m", "reviewed implementation")
	writeRestoreFixture(t, repository, "uncommitted.bin", "\x00\xff\x01")
	var err error
	record.SnapshotSHA, err = CaptureSnapshot(context.Background(), repository, record.RunID, record.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	record.PatchDigest = recoveryTestPatchDigest(t, repository, record)
	// This fixture's untracked working file is moved aside, not destroyed, so
	// restoration must obtain it from retained objects rather than this path.
	if err := os.Rename(filepath.Join(repository, "uncommitted.bin"), filepath.Join(t.TempDir(), "saved.bin")); err != nil {
		t.Fatal(err)
	}
	recoveryTestGit(t, repository, "checkout", "main")
	writeRestoreFixture(t, repository, "new-main.txt", "new upstream work")
	recoveryTestGit(t, repository, "add", ".")
	recoveryTestGit(t, repository, "commit", "-m", "advance main")
	main := recoveryTestGit(t, repository, "rev-parse", "HEAD")
	index := filepath.Join(repository, ".git", "index")
	before, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreSnapshot(context.Background(), repository, record, main, "recovered", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got := recoveryTestGit(t, repository, "rev-parse", "recovered^"); got != main {
		t.Fatalf("restore did not use current main: %s", got)
	}
	testVerifyPreparedRestoration(t, repository, record, restored, main)
	for path, want := range map[string]string{"shared.txt": "ours", "reviewed.txt": "reviewed work", "uncommitted.bin": "\x00\xff\x01", "new-main.txt": "new upstream work"} {
		if got := recoveryTestGit(t, repository, "show", restored+":"+path); got != want {
			t.Fatalf("restored %s = %q, want %q", path, got, want)
		}
	}
	if got := recoveryTestGit(t, repository, "rev-parse", "HEAD"); got != main {
		t.Fatalf("restore moved current HEAD: %s", got)
	}
	after, err := os.ReadFile(index)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("restore changed current index: %v", err)
	}
	if _, err := RestoreSnapshot(context.Background(), repository, record, main, "recovered", 1<<20); err == nil {
		t.Fatal("existing branch was overwritten")
	}
	if got := recoveryTestGit(t, repository, "rev-parse", "recovered"); got != restored {
		t.Fatalf("existing recovery branch changed: %s", got)
	}
	writeRestoreFixture(t, repository, "shared.txt", "theirs\n")
	recoveryTestGit(t, repository, "add", ".")
	recoveryTestGit(t, repository, "commit", "-m", "conflicting main")
	conflictingMain := recoveryTestGit(t, repository, "rev-parse", "HEAD")
	if commit, err := RestoreSnapshot(context.Background(), repository, record, conflictingMain, "conflict", 1<<20); err == nil || commit != "" {
		t.Fatalf("conflicted restore acknowledged: %q %v", commit, err)
	}
	if got := recoveryTestGit(t, repository, "for-each-ref", "--format=%(refname)", "refs/heads/conflict"); got != "" {
		t.Fatalf("conflict created a branch: %s", got)
	}
}

func testVerifyPreparedRestoration(t *testing.T, repository string, record Record, restored, main string) {
	t.Helper()
	if err := VerifyRestoredCommit(context.Background(), repository, record, restored, 1<<20); err != nil {
		t.Fatalf("verified restoration refused on replay: %v", err)
	}
	if err := VerifyRestoredCommit(context.Background(), repository, record, main, 1<<20); err == nil {
		t.Fatal("unrestored main accepted as completed restoration")
	}
	tree := recoveryTestGit(t, repository, "rev-parse", restored+"^{tree}")
	merge := recoveryTestGit(t, repository, "commit-tree", tree, "-p", main, "-p", record.SnapshotSHA, "-m", "Restore retained implementation for "+record.RunID)
	if err := VerifyRestoredCommit(context.Background(), repository, record, merge, 1<<20); err == nil {
		t.Fatal("merge commit accepted as a single-parent restoration")
	}
	wrongTree := recoveryTestGit(t, repository, "rev-parse", main+"^{tree}")
	forged := recoveryTestGit(t, repository, "commit-tree", wrongTree, "-p", main, "-m", "Restore retained implementation for "+record.RunID)
	if err := VerifyRestoredCommit(context.Background(), repository, record, forged, 1<<20); err == nil {
		t.Fatal("matching message substituted for verified restored content")
	}
}

func writeRestoreFixture(t *testing.T, repository, path, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repository, path), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
