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

func TestIntegrationCaptureSnapshotPreservesWorktreeAndIndex(t *testing.T) {
	testdep.Require(t, "git")
	repository := t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	for name, content := range map[string]string{"tracked.txt": "base", ".gitignore": "ignored.out\n"} {
		if err := os.WriteFile(filepath.Join(repository, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	recoveryTestGit(t, repository, "add", ".")
	recoveryTestGit(t, repository, "commit", "-m", "base")
	head := recoveryTestGit(t, repository, "rev-parse", "HEAD")
	for name, content := range map[string]string{"tracked.txt": "changed\r\n", "new.bin": "\x00\xff\x01", "ignored.out": "generated"} {
		if err := os.WriteFile(filepath.Join(repository, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	indexPath := filepath.Join(repository, ".git", "index")
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	// A clean driver can replace content with a pointer (as LFS does). A
	// self-contained archive must retain the actual worktree bytes instead.
	recoveryTestGit(t, repository, "config", "filter.recovery-test.clean", "git hash-object --stdin")
	recoveryTestGit(t, repository, "config", "filter.recovery-test.required", "true")
	attributes := []byte("* filter=recovery-test text eol=lf working-tree-encoding=UTF-16LE\n")
	if err := os.WriteFile(filepath.Join(repository, ".git", "info", "attributes"), attributes, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := CaptureSnapshot(context.Background(), repository, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	var captured bytes.Buffer
	if err := recoveryGit(context.Background(), repository, &captured, "cat-file", "blob", snapshot+":tracked.txt"); err != nil || captured.String() != "changed\r\n" {
		t.Fatalf("tracked bytes transformed: %q %v", captured.String(), err)
	}
	if got := recoveryTestGit(t, repository, "show", snapshot+":new.bin"); got != "\x00\xff\x01" {
		t.Fatalf("untracked binary lost: %q", got)
	}
	if got := recoveryTestGit(t, repository, "ls-tree", "--name-only", snapshot, "ignored.out"); got != "" {
		t.Fatalf("ignored output captured: %q", got)
	}
	if got := recoveryTestGit(t, repository, "rev-parse", "HEAD"); got != head {
		t.Fatalf("capture changed HEAD: %s", got)
	}
	after, err := os.ReadFile(indexPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("capture changed caller's index: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(repository, "tracked.txt")); err != nil || string(got) != "changed\r\n" {
		t.Fatalf("capture changed working file: %q %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(repository, ".git", "info", "attributes")); err != nil || !bytes.Equal(got, attributes) {
		t.Fatalf("capture changed source attributes: %q %v", got, err)
	}
}
