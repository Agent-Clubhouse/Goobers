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

func TestIntegrationAdoptRecoveryPreservesUnrelatedWork(t *testing.T) {
	testdep.Require(t, "git")
	for _, mode := range []string{"valid", "dirty", "staged", "untracked", "ignored-collision", "foreign-branch", "advanced", "diverged", "detached"} {
		t.Run(mode, func(t *testing.T) {
			repository := t.TempDir()
			recoveryTestGit(t, repository, "init", "--initial-branch=receiving")
			writeRestoreFixture(t, repository, "tracked", "original")
			writeRestoreFixture(t, repository, ".gitignore", "implementation\n")
			recoveryTestGit(t, repository, "add", ".")
			recoveryTestGit(t, repository, "commit", "-m", "base")
			base := recoveryTestGit(t, repository, "rev-parse", "HEAD")
			recoveryTestGit(t, repository, "checkout", "-b", "restored")
			writeRestoreFixture(t, repository, "implementation", "recovered")
			recoveryTestGit(t, repository, "add", "--force", "implementation")
			recoveryTestGit(t, repository, "commit", "-m", "restored")
			restored := recoveryTestGit(t, repository, "rev-parse", "HEAD")
			recoveryTestGit(t, repository, "checkout", "receiving")
			switch mode {
			case "dirty", "staged":
				writeRestoreFixture(t, repository, "tracked", "operator work")
				if mode == "staged" {
					recoveryTestGit(t, repository, "add", "tracked")
				}
			case "untracked":
				writeRestoreFixture(t, repository, "untracked", "operator work")
			case "ignored-collision":
				writeRestoreFixture(t, repository, "implementation", "operator work")
			case "foreign-branch":
				recoveryTestGit(t, repository, "checkout", "-b", "foreign")
			case "advanced", "diverged":
				recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "other work")
			case "detached":
				recoveryTestGit(t, repository, "checkout", "--detach", base)
			}
			before := recoveryTestGit(t, repository, "rev-parse", "HEAD")
			indexBefore := recoveryTestGit(t, repository, "ls-files", "--stage")
			filesBefore := make(map[string][]byte)
			for _, name := range []string{"tracked", "untracked", "implementation"} {
				data, err := os.ReadFile(filepath.Join(repository, name))
				if err == nil {
					filesBefore[name] = data
				} else if !os.IsNotExist(err) {
					t.Fatal(err)
				}
			}
			expected := base
			if mode == "diverged" {
				expected = before
			}
			err := AdoptRestoredCommit(context.Background(), repository, "receiving", expected, restored)
			if mode == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				if got, err := os.ReadFile(filepath.Join(repository, "implementation")); err != nil || string(got) != "recovered" {
					t.Fatalf("restored work not checked out: %q %v", got, err)
				}
				if err := AdoptRestoredCommit(context.Background(), repository, "receiving", base, restored); err != nil {
					t.Fatalf("acknowledged adoption retry refused: %v", err)
				}
				return
			}
			if err == nil || recoveryTestGit(t, repository, "rev-parse", "HEAD") != before {
				t.Fatalf("unsafe adoption changed HEAD: %v", err)
			}
			if recoveryTestGit(t, repository, "ls-files", "--stage") != indexBefore {
				t.Fatal("refused adoption changed staged work")
			}
			for _, name := range []string{"tracked", "untracked", "implementation"} {
				data, readErr := os.ReadFile(filepath.Join(repository, name))
				if want, existed := filesBefore[name]; existed {
					if readErr != nil || !bytes.Equal(data, want) {
						t.Fatalf("refused adoption changed %s: %q %v", name, data, readErr)
					}
				} else if !os.IsNotExist(readErr) {
					t.Fatalf("refused adoption created %s: %v", name, readErr)
				}
			}
		})
	}
}
