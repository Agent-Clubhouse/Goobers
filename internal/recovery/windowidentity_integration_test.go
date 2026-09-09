//go:build integration

package recovery

import (
	"context"
	"testing"
	"time"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationSnapshotWindowsWithinOneSecondRemainDistinct(t *testing.T) {
	testdep.Require(t, "git")
	repository, inventory := t.TempDir(), t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "base")
	template := storageTestRecord()
	start := template.CreatedAt.Truncate(time.Second).Add(time.Nanosecond)
	finish := start.Add(time.Millisecond)
	var snapshots []Record
	for _, stamp := range []time.Time{start, finish, finish.In(time.FixedZone("alternate", 3600))} {
		prepared, err := PrepareRecord(context.Background(), repository, template.RepositoryKey, template.RunID, "main", stamp, stamp.Add(30*24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		published, _, err := PublishToInventory(context.Background(), repository, inventory, []string{repository}, prepared, 2, 1<<20)
		if err != nil {
			t.Fatalf("same-second capture window failed publication: %v", err)
		}
		snapshots = append(snapshots, published)
	}
	if snapshots[0].SnapshotSHA == snapshots[1].SnapshotSHA {
		t.Fatal("distinct capture windows collided at Git timestamp precision")
	}
	if snapshots[1] != snapshots[2] {
		t.Fatal("equivalent timestamp retry did not reuse terminal snapshot")
	}
}
