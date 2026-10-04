//go:build integration

package recovery

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

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
