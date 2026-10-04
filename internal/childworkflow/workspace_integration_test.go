//go:build integration

package childworkflow

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func childSnapshotGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=Snapshot Test", "-c", "user.email=snapshot@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	data, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, data)
	}
	return strings.TrimSpace(string(data))
}

func childSnapshotWrite(t *testing.T, dir, name, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationWorkspaceCoordinatorRetainsFirstYieldedCapture(t *testing.T) {
	testdep.Require(t, "git")
	service, authority, databasePath := submissionFixture(t)
	submission, err := service.Submit(t.Context(), authority.current.Origin, SubmissionRequest{InvocationKey: "child", Source: []byte(validProposal)})
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	childSnapshotGit(t, parent, "init", "--initial-branch=main")
	childSnapshotWrite(t, parent, "main.txt", "base")
	childSnapshotWrite(t, parent, "deleted.txt", "delete in parent")
	childSnapshotGit(t, parent, "add", ".")
	childSnapshotGit(t, parent, "commit", "-m", "base")
	manager, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.WorkingCopy(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	coordinator := &WorkspaceCoordinator{Queue: service.Queue, Worktrees: manager}
	if _, err := coordinator.Prepare(t.Context(), submission.Child, parent); !errors.Is(err, triggerqueue.ErrChildSnapshotPending) {
		t.Fatalf("live parent prepared before yield: %v", err)
	}
	childSnapshotWrite(t, parent, "main.txt", "captured dirty state")
	childSnapshotWrite(t, parent, "new.txt", "untracked state")
	childSnapshotWrite(t, parent, ".goobers/credential", "secret runtime")
	childSnapshotWrite(t, parent, "auth/token", "secret credential")
	if err := os.Remove(filepath.Join(parent, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	head := childSnapshotGit(t, parent, "rev-parse", "HEAD")
	index, err := os.ReadFile(filepath.Join(parent, ".git/index"))
	if err != nil {
		t.Fatal(err)
	}
	key := (providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}).CanonicalKey()
	yielded := YieldedWorkspace{Path: parent, RepoURL: parent, RepositoryKey: key, Policy: recovery.SnapshotPolicy{ExcludedPaths: []string{"auth"}}}
	if err := coordinator.Capture(t.Context(), submission.Child, yielded); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(parent, ".git/index"))
	if err != nil || !bytes.Equal(after, index) {
		t.Fatal("capture changed parent index")
	}
	if childSnapshotGit(t, parent, "rev-parse", "HEAD") != head {
		t.Fatal("capture changed parent HEAD")
	}
	childSnapshotWrite(t, parent, "main.txt", "later parent state must not be recaptured")
	if err := service.Queue.Close(); err != nil {
		t.Fatal(err)
	}
	service.Queue, err = triggerqueue.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	coordinator.Queue = service.Queue
	if err := coordinator.Capture(t.Context(), submission.Child, yielded); err != nil {
		t.Fatal(err)
	}
	admission, err := coordinator.Prepare(t.Context(), submission.Child, parent)
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := manager.AdoptChildFromSnapshot(t.Context(), worktree.ChildOptions{RepoURL: parent, RunID: admission.WorkspaceID, OwnerRunID: submission.Child.RunID, Gaggle: submission.Child.Identity.Gaggle, SnapshotSHA: admission.ForkSHA})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"main.txt": "captured dirty state", "new.txt": "untracked state"} {
		data, err := os.ReadFile(filepath.Join(adopted.Path, name))
		if err != nil || string(data) != want {
			t.Fatalf("%s=%q err=%v", name, data, err)
		}
	}
	for _, name := range []string{"deleted.txt", ".goobers/credential", "auth/token"} {
		if _, err := os.Stat(filepath.Join(adopted.Path, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("forbidden/deleted path inherited: %s %v", name, err)
		}
	}
	childSnapshotWrite(t, adopted.Path, "main.txt", "child progress")
	retry, err := coordinator.Prepare(t.Context(), submission.Child, parent)
	if err != nil || *retry != *admission {
		t.Fatalf("prepare retry=%+v %v", retry, err)
	}
	data, err := os.ReadFile(filepath.Join(adopted.Path, "main.txt"))
	if err != nil || string(data) != "child progress" {
		t.Fatal("prepare retry reset child edits")
	}
	if _, err := coordinator.Prepare(t.Context(), submission.Child, parent+"/other"); err == nil {
		t.Fatal("repository binding changed")
	}
	if _, err := submissionDB(t, databasePath).Exec(`DELETE FROM child_snapshots`); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Capture(t.Context(), submission.Child, yielded); !errors.Is(err, triggerqueue.ErrChildSnapshotUnavailable) {
		t.Fatalf("lost carrier was recaptured: %v", err)
	}
}
