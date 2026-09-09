//go:build integration

package recovery

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationSuccessiveSnapshotsRetainIndependentArchives(t *testing.T) {
	testdep.Require(t, "git")
	ctx := context.Background()
	repository := t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "base")
	base := recoveryTestGit(t, repository, "rev-parse", "HEAD")
	var retained []Record
	var directories []string
	for _, payload := range []string{"first attempt", "later attempt"} {
		if err := os.WriteFile(filepath.Join(repository, "change.txt"), []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
		record := storageTestRecord()
		record.BaseSHA = base
		var err error
		record.SnapshotSHA, err = CaptureSnapshot(ctx, repository, record.RunID, record.CreatedAt)
		if err != nil {
			t.Fatal(err)
		}
		record.Ref, err = RefForSnapshot(record.RunID, record.SnapshotSHA)
		if err != nil {
			t.Fatal(err)
		}
		record.PatchDigest = recoveryTestPatchDigest(t, repository, record)
		directory := t.TempDir()
		published, err := PublishRetainedState(ctx, repository, directory, []string{repository}, record, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		retained = append(retained, published)
		directories = append(directories, directory)
	}
	if retained[0].Ref == retained[1].Ref || retained[0].RunID != retained[1].RunID {
		t.Fatal("successive snapshots did not retain independent identities under one owner")
	}
	for i, record := range retained {
		if got := recoveryTestGit(t, repository, "rev-parse", record.Ref); got != record.SnapshotSHA {
			t.Fatal("later capture replaced an earlier retention ref")
		}
		// Restore each archive into a separate repository: neither restoration
		// can accidentally borrow the other attempt's objects or working files.
		restored := t.TempDir()
		recoveryTestGit(t, restored, "init", "--initial-branch=main")
		if err := ImportSnapshotBundle(ctx, restored, filepath.Join(directories[i], BundleFileName), record, 1<<20); err != nil {
			t.Fatal(err)
		}
		commit, err := RestoreSnapshot(ctx, restored, record, base, "recovered", 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"first attempt", "later attempt"}[i]
		if got := recoveryTestGit(t, restored, "show", commit+":change.txt"); got != want {
			t.Fatalf("restored wrong attempt: got %q want %q", got, want)
		}
	}
}
