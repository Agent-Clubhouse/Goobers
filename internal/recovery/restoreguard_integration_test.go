//go:build integration

package recovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/gooberassets"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationRestoreQuarantinesRuntimeAssetChanges(t *testing.T) {
	testdep.Require(t, "git")
	ctx := context.Background()
	repository := t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "base")
	record := storageTestRecord()
	record.BaseSHA = recoveryTestGit(t, repository, "rev-parse", "HEAD")
	assets := filepath.Join(repository, gooberassets.WorkspaceDir)
	if err := os.Mkdir(assets, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assets, "private.txt"), []byte("runtime-only fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	recoveryTestGit(t, repository, "add", ".")
	recoveryTestGit(t, repository, "commit", "-m", "runtime asset collision")
	record.SnapshotSHA = recoveryTestGit(t, repository, "rev-parse", "HEAD")
	record.PatchDigest = recoveryTestPatchDigest(t, repository, record)
	if commit, err := RestoreSnapshot(ctx, repository, record, record.BaseSHA, "recovered", 1<<20); !errors.Is(err, ErrReservedRecoveryPath) || commit != "" {
		t.Fatalf("runtime assets restored: %q %v", commit, err)
	}
	if ref := recoveryTestGit(t, repository, "for-each-ref", "--format=%(refname)", "refs/heads/recovered"); ref != "" {
		t.Fatal("rejected restore created an operator branch")
	}
	if ref := recoveryTestGit(t, repository, "rev-parse", record.Ref); ref != record.SnapshotSHA {
		t.Fatal("rejected restore discarded retained forensic state")
	}
}
