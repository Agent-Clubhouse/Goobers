//go:build integration

package recovery

import (
	"bytes"
	"github.com/goobers/goobers/test/testsupport/testdep"
	"os"
	"path/filepath"
	"testing"
)

func TestIntegrationChildPublicationPreservesAncestryOmissionsAndWorkspace(t *testing.T) {
	testdep.Require(t, "git")
	repository, key, at := childSnapshotFixture(t)
	childSnapshotWrite(t, repository, "private/template.txt", "tracked baseline\n")
	recoveryTestGit(t, repository, "add", "private/template.txt")
	recoveryTestGit(t, repository, "commit", "-m", "tracked omitted baseline")
	base := recoveryTestGit(t, repository, "rev-parse", "HEAD")
	fork := captureChildFixture(t, repository, key, "parent", at, SnapshotPolicy{ExcludedPaths: []string{"private"}})
	recoveryTestGit(t, repository, "checkout", "-b", "goobers/children/child", fork.Record.SnapshotSHA)
	childSnapshotWrite(t, repository, "private/template.txt", "injected secret\n")
	childSnapshotWrite(t, repository, "private/new-token", "new secret\n")
	childSnapshotWrite(t, repository, "tracked.txt", "child committed\n")
	recoveryTestGit(t, repository, "add", "tracked.txt")
	recoveryTestGit(t, repository, "commit", "-m", "child implementation")
	childSnapshotWrite(t, repository, "new.txt", "child dirty\n")
	childSnapshotWrite(t, repository, ".goobers/token", "runtime secret\n")
	head := recoveryTestGit(t, repository, "rev-parse", "HEAD")
	indexPath := filepath.Join(repository, ".git", "index")
	beforeIndex, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	_, sha, err := CaptureChildPublication(t.Context(), repository, "child", fork, at)
	if err != nil {
		t.Fatal(err)
	}
	_, again, err := CaptureChildPublication(t.Context(), repository, "child", fork, at)
	if err != nil || again != sha {
		t.Fatal("publication replay changed", sha, again, err)
	}
	recoveryTestGit(t, repository, "merge-base", "--is-ancestor", base, sha)
	if got := recoveryTestGit(t, repository, "rev-parse", sha+"^"); got != head {
		t.Fatal("publication lost real child ancestry", got, head)
	}
	if got := recoveryTestGit(t, repository, "show", sha+":private/template.txt"); got != "tracked baseline" {
		t.Fatal("omission replaced original content", got)
	}
	for _, name := range []string{"private/new-token", ".goobers/token"} {
		if got := recoveryTestGit(t, repository, "ls-tree", "--name-only", sha, "--", name); got != "" {
			t.Fatal("publication captured runtime bytes", got)
		}
	}
	if got := recoveryTestGit(t, repository, "show", sha+":new.txt"); got != "child dirty" {
		t.Fatal("dirty child work missing", got)
	}
	if got := recoveryTestGit(t, repository, "rev-parse", "HEAD"); got != head {
		t.Fatal("publication moved HEAD")
	}
	afterIndex, err := os.ReadFile(indexPath)
	if err != nil || !bytes.Equal(beforeIndex, afterIndex) {
		t.Fatal("publication moved index", err)
	}
	if data, _ := os.ReadFile(filepath.Join(repository, "private/template.txt")); string(data) != "injected secret\n" {
		t.Fatal("publication changed private worktree")
	}
}
