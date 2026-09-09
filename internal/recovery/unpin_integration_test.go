//go:build integration

package recovery

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationDeleteRecoveryPinPreservesBranchesAndConflictingRefs(t *testing.T) {
	testdep.Require(t, "git")
	for _, mode := range []string{"direct", "conflict", "symbolic", "dangling-symbolic", "cancelled", "operator-branch", "invalid-repository"} {
		t.Run(mode, func(t *testing.T) {
			repository := t.TempDir()
			recoveryTestGit(t, repository, "init", "--initial-branch=main")
			recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "base")
			record := storageTestRecord()
			record.BaseSHA = recoveryTestGit(t, repository, "rev-parse", "HEAD")
			recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "implementation")
			record.SnapshotSHA = recoveryTestGit(t, repository, "rev-parse", "HEAD")
			record.Ref, _ = RefForSnapshot(record.RunID, record.SnapshotSHA)
			recoveryTestGit(t, repository, "branch", "operator-branch")
			recoveryTestGit(t, repository, "update-ref", record.Ref, record.SnapshotSHA)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "conflict":
				recoveryTestGit(t, repository, "update-ref", record.Ref, record.BaseSHA)
			case "symbolic":
				recoveryTestGit(t, repository, "symbolic-ref", record.Ref, "refs/heads/operator-branch")
			case "dangling-symbolic":
				recoveryTestGit(t, repository, "symbolic-ref", record.Ref, "refs/heads/missing")
			case "cancelled":
				cancel()
			case "operator-branch":
				record.Ref = "refs/heads/operator-branch"
			}
			deletionRepository := repository
			if mode == "invalid-repository" {
				deletionRepository = t.TempDir()
			}
			err := DeleteSnapshotRef(ctx, deletionRepository, record)
			if mode == "direct" {
				if err != nil {
					t.Fatal(err)
				}
				if refs := recoveryTestGit(t, repository, "for-each-ref", "--format=%(refname)", record.Ref); refs != "" {
					t.Fatalf("acknowledged pin deletion left ref: %s", refs)
				}
				if err := DeleteSnapshotRef(ctx, repository, record); err != nil {
					t.Fatalf("identical deletion retry failed: %v", err)
				}
			} else if err == nil {
				t.Fatal("unsafe pin deletion acknowledged")
			}
			for _, branch := range []string{"main", "operator-branch"} {
				if got := recoveryTestGit(t, repository, "rev-parse", branch); got != record.SnapshotSHA {
					t.Fatalf("pin deletion changed %s: %s", branch, got)
				}
			}
			if mode == "symbolic" && recoveryTestGit(t, repository, "symbolic-ref", record.Ref) != "refs/heads/operator-branch" {
				t.Fatal("symbolic alias was removed or replaced")
			}
			if mode == "dangling-symbolic" && recoveryTestGit(t, repository, "symbolic-ref", record.Ref) != "refs/heads/missing" {
				t.Fatal("dangling symbolic alias was removed or replaced")
			}
			if mode == "conflict" && recoveryTestGit(t, repository, "rev-parse", record.Ref) != record.BaseSHA {
				t.Fatal("conflicting ref was removed or replaced")
			}
		})
	}
}

func TestIntegrationDeleteRecoveryPinIgnoresInheritedGitDirectory(t *testing.T) {
	testdep.Require(t, "git")
	repository := t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "base")
	record := storageTestRecord()
	record.BaseSHA = recoveryTestGit(t, repository, "rev-parse", "HEAD")
	record.SnapshotSHA = record.BaseSHA
	recoveryTestGit(t, repository, "update-ref", record.Ref, record.SnapshotSHA)
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "unrelated.git"))
	if err := DeleteSnapshotRef(context.Background(), repository, record); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repository, ".git", record.Ref)); !os.IsNotExist(err) {
		t.Fatalf("inherited environment redirected deletion: %v", err)
	}
}
