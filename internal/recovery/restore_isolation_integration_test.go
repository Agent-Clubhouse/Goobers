//go:build integration

package recovery

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestIntegrationRestoreAddAddDoesNotWriteLiveCheckout(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		name := "identical"
		if conflict {
			name = "conflict"
		}
		t.Run(name, func(t *testing.T) {
			repository, key, at := childSnapshotFixture(t)
			before := captureChildFixture(t, repository, key, "before", at, SnapshotPolicy{})
			var inputs []ChildSnapshot
			for index, label := range []string{"left", "right"} {
				workspace := filepath.Join(t.TempDir(), label)
				recoveryTestGit(t, repository, "worktree", "add", "--detach", workspace, before.Record.SnapshotSHA)
				value := "same\n"
				if conflict && index == 1 {
					value = "different\n"
				}
				childSnapshotWrite(t, workspace, "shared.txt", value)
				inputs = append(inputs, captureChildFixture(t, workspace, key, label, at, SnapshotPolicy{}))
			}
			commit, err := RestoreSnapshot(t.Context(), repository, inputs[1].Record, inputs[0].Record.SnapshotSHA, "restored", 1<<20)
			if conflict {
				if !errors.Is(err, ErrIncompatibleSnapshot) || commit != "" {
					t.Fatal("add/add conflict was not refused", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if got := recoveryTestGit(t, repository, "show", commit+":shared.txt"); got != "same" {
					t.Fatal("restored tree lost the merged file", got)
				}
			}
			if err := CheckChildSnapshotCurrent(t.Context(), repository, before); err != nil {
				t.Fatal("private-index merge modified the live checkout", err)
			}
		})
	}
}
