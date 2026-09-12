//go:build integration

package recovery

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationCaptureSnapshotPreservesSparseFiles(t *testing.T) {
	testdep.Require(t, "git")
	repository := t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	for _, directory := range []string{"visible", "hidden"} {
		if err := os.Mkdir(filepath.Join(repository, directory), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repository, directory, "file.txt"), []byte(directory), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	recoveryTestGit(t, repository, "add", ".")
	recoveryTestGit(t, repository, "commit", "-m", "base")
	recoveryTestGit(t, repository, "sparse-checkout", "init", "--cone", "--sparse-index")
	recoveryTestGit(t, repository, "sparse-checkout", "set", "visible")
	if _, err := os.Stat(filepath.Join(repository, "hidden", "file.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture did not exclude hidden file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repository, "visible", "file.txt"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Preserve staged content outside the cone too, not merely the old HEAD
	// blob. This content has no working file from which it could be recaptured.
	var staged bytes.Buffer
	if err := recoveryGitIO(context.Background(), repository, &staged, strings.NewReader("staged hidden"), nil, "hash-object", "-w", "--stdin"); err != nil {
		t.Fatal(err)
	}
	recoveryTestGit(t, repository, "update-index", "--cacheinfo", "100644,"+strings.TrimSpace(staged.String())+",hidden/file.txt")
	recoveryTestGit(t, repository, "update-index", "--skip-worktree", "hidden/file.txt")
	indexPath := filepath.Join(repository, ".git", "index")
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := CaptureSnapshot(context.Background(), repository, "sparse-run", storageTestRecord().CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if got := recoveryTestGit(t, repository, "show", snapshot+":hidden/file.txt"); got != "staged hidden" {
		t.Fatalf("sparse file lost: %q", got)
	}
	if got := recoveryTestGit(t, repository, "show", snapshot+":visible/file.txt"); got != "changed" {
		t.Fatalf("visible edit lost: %q", got)
	}
	after, err := os.ReadFile(indexPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("capture changed source sparse index: %v", err)
	}
}
