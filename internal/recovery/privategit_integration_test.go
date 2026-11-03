//go:build integration

package recovery

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

// Recovery custody must not need free space in the platform temp root: in a
// tmp:ephemeral pod that is a small tmpfs that Go builds could fill.
func TestIntegrationPrivateGitDirectoryLivesInGitDirNotTempRoot(t *testing.T) {
	testdep.Require(t, "git")
	repository := t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing", "tmp"))
	directory, err := privateGitDirectory(context.Background(), repository, "goobers-recovery-index-*")
	if err != nil {
		t.Fatalf("privateGitDirectory: %v", err)
	}

	if want := filepath.Join(repository, ".git") + string(filepath.Separator); !strings.HasPrefix(directory, want) {
		// macOS resolves /var to /private/var; compare by suffix as well.
		if !strings.Contains(directory, filepath.Join(".git", "goobers-recovery-index-")) {
			t.Fatalf("directory = %q, want inside %q", directory, want)
		}
	}
}

func TestIntegrationPrivateGitSnapshotSupportsLongWindowsPaths(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows Git path-length regression")
	}
	testdep.Require(t, "git")
	parent := t.TempDir()
	padding := 210 - len(parent) - 1
	if padding < 1 {
		t.Fatalf("temporary root is too long for the repository fixture: %s", parent)
	}
	repository := filepath.Join(parent, strings.Repeat("r", padding))
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	recoveryTestGit(t, repository, "-c", "core.longpaths=true", "init", "--initial-branch=main")
	recoveryTestGit(t, repository, "-c", "core.longpaths=true", "commit", "--allow-empty", "-m", "base")
	if err := os.WriteFile(filepath.Join(repository, "change.txt"), []byte("retained bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := CaptureSnapshot(t.Context(), repository, "long-private-git", time.Unix(1, 0))
	if err != nil {
		t.Fatalf("capture through private Git metadata beyond MAX_PATH: %v", err)
	}
	if got := recoveryTestGit(t, repository, "-c", "core.longpaths=true", "show", snapshot+":change.txt"); got != "retained bytes" {
		t.Fatalf("captured bytes = %q", got)
	}
	if got := recoveryTestGit(t, repository, "-c", "core.longpaths=true", "status", "--short"); got != "?? change.txt" {
		t.Fatalf("capture changed the source index: %q", got)
	}
}
