//go:build integration

package recovery

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func recoveryTestGit(t *testing.T, repository string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repository}, args...)...)
	command.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Recovery Test", "GIT_AUTHOR_EMAIL=recovery@example.invalid", "GIT_COMMITTER_NAME=Recovery Test", "GIT_COMMITTER_EMAIL=recovery@example.invalid")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func TestIntegrationPinCommitPreservesSnapshotAcrossBranchDeletion(t *testing.T) {
	testdep.Require(t, "git")
	repository := t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "base")
	record := storageTestRecord()
	record.BaseSHA = recoveryTestGit(t, repository, "rev-parse", "HEAD")
	recoveryTestGit(t, repository, "checkout", "-b", "implementation")
	if err := os.WriteFile(filepath.Join(repository, "change.bin"), []byte{0, 1, 2, 255}, 0o600); err != nil {
		t.Fatal(err)
	}
	recoveryTestGit(t, repository, "add", "change.bin")
	recoveryTestGit(t, repository, "commit", "-m", "implementation")
	record.SnapshotSHA = recoveryTestGit(t, repository, "rev-parse", "HEAD")
	record.PatchDigest = recoveryTestPatchDigest(t, repository, record)
	// A caller launched from another Git operation must not pin that operation's
	// repository instead of the explicit target. Verification commands below
	// run after these overrides have been restored.
	t.Run("inherited repository overrides", func(t *testing.T) {
		t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "missing.git"))
		t.Setenv("GIT_WORK_TREE", t.TempDir())
		if err := PinCommit(context.Background(), repository, record); err != nil {
			t.Fatalf("inherited Git environment changed target: %v", err)
		}
	})
	for range 2 {
		if err := PinCommit(context.Background(), repository, record); err != nil {
			t.Fatal(err)
		}
	}
	recoveryTestGit(t, repository, "checkout", "main")
	recoveryTestGit(t, repository, "branch", "-D", "implementation")
	recoveryTestGit(t, repository, "reflog", "expire", "--expire=now", "--all")
	recoveryTestGit(t, repository, "gc", "--prune=now")
	if got := recoveryTestGit(t, repository, "rev-parse", record.Ref); got != record.SnapshotSHA {
		t.Fatalf("snapshot ref lost: %s", got)
	}
	recoveryTestGit(t, repository, "cat-file", "-e", record.SnapshotSHA+":change.bin")
	conflict := record
	conflict.SnapshotSHA = record.BaseSHA
	conflict.PatchDigest = recoveryTestPatchDigest(t, repository, conflict)
	if err := PinCommit(context.Background(), repository, conflict); !errors.Is(err, ErrRecordConflict) {
		t.Fatalf("conflicting pin accepted: %v", err)
	}
	if got := recoveryTestGit(t, repository, "rev-parse", record.Ref); got != record.SnapshotSHA {
		t.Fatalf("conflict replaced original snapshot: %s", got)
	}
}

func TestIntegrationPinCommitRefusesMissingUnrelatedAndCancelledSnapshots(t *testing.T) {
	testdep.Require(t, "git")
	repository := t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "base")
	base := recoveryTestGit(t, repository, "rev-parse", "HEAD")
	recoveryTestGit(t, repository, "checkout", "--orphan", "unrelated")
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "unrelated history")
	unrelated := recoveryTestGit(t, repository, "rev-parse", "HEAD")
	for _, name := range []string{"missing", "unrelated", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			record := storageTestRecord()
			record.RunID = name
			record.Ref = "refs/goobers/recovery/" + name
			record.BaseSHA = base
			record.SnapshotSHA = base
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch name {
			case "missing":
				record.SnapshotSHA = strings.Repeat("f", len(base))
			case "unrelated":
				record.SnapshotSHA = unrelated
			case "cancelled":
				cancel()
			}
			if err := PinCommit(ctx, repository, record); err == nil {
				t.Fatal("invalid or cancelled snapshot accepted")
			}
			if refs := recoveryTestGit(t, repository, "for-each-ref", "--format=%(refname)", record.Ref); refs != "" {
				t.Fatalf("failed pin left a recovery ref: %s", refs)
			}
		})
	}
}

func TestIntegrationPinCommitDoesNotRetainSymbolicAliases(t *testing.T) {
	testdep.Require(t, "git")
	repository := t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "base")
	record := storageTestRecord()
	record.BaseSHA = recoveryTestGit(t, repository, "rev-parse", "HEAD")
	record.SnapshotSHA = record.BaseSHA
	record.PatchDigest = recoveryTestPatchDigest(t, repository, record)
	recoveryTestGit(t, repository, "symbolic-ref", record.Ref, "refs/heads/main")
	if err := PinCommit(context.Background(), repository, record); err != nil {
		t.Fatal(err)
	}
	if target := recoveryTestGit(t, repository, "for-each-ref", "--format=%(symref)", record.Ref); target != "" {
		t.Fatalf("pin acknowledged a symbolic alias instead of an independent retention ref: %s", target)
	}
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "advance main")
	if got := recoveryTestGit(t, repository, "rev-parse", record.Ref); got != record.SnapshotSHA {
		t.Fatalf("advancing main changed the retained snapshot: %s", got)
	}
	conflict := record
	conflict.RunID = "alias-conflict"
	conflict.Ref = "refs/goobers/recovery/alias-conflict"
	recoveryTestGit(t, repository, "symbolic-ref", conflict.Ref, "refs/heads/main")
	mainBefore := recoveryTestGit(t, repository, "rev-parse", "main")
	if err := PinCommit(context.Background(), repository, conflict); !errors.Is(err, ErrRecordConflict) {
		t.Fatalf("conflicting symbolic pin accepted: %v", err)
	}
	if got := recoveryTestGit(t, repository, "rev-parse", "main"); got != mainBefore {
		t.Fatalf("pin rewrote main: %s", got)
	}
}

func recoveryTestPatchDigest(t *testing.T, repository string, record Record) string {
	t.Helper()
	digest, err := WriteSnapshotPatch(context.Background(), repository, record.BaseSHA, record.SnapshotSHA, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}
