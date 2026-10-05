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
	"time"

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
	// A terminal result must return commits as well as dirty child work. Its
	// prerequisite stays the original fork, so committed changes are not lost.
	childSnapshotGit(t, adopted.Path, "add", "main.txt")
	childSnapshotGit(t, adopted.Path, "commit", "-m", "child commit")
	childSnapshotWrite(t, adopted.Path, "later.txt", "uncommitted child work")
	childSnapshotWrite(t, adopted.Path, ".goobers/token", "must remain private")
	childWorkspace := yielded
	childWorkspace.Path = adopted.Path
	terminal := TerminalResultInput{State: triggerqueue.ChildCompleted, FinishedAt: submission.Child.AcceptedAt.Add(time.Minute), Summary: "Updated implementation", References: []string{"artifact:summary"}}
	result, err := coordinator.CaptureResult(t.Context(), submission.Child, &childWorkspace, terminal)
	if err != nil {
		t.Fatal(err)
	}
	if result.Snapshot == nil || result.Snapshot.Record.BaseSHA != admission.ForkSHA || result.ResultRef == "" || result.WorkspaceRef == "" {
		t.Fatalf("result=%+v", result)
	}
	if got := childSnapshotGit(t, adopted.Path, "show", result.Snapshot.Record.SnapshotSHA+":main.txt"); got != "child progress" {
		t.Fatalf("committed result=%q", got)
	}
	if got := childSnapshotGit(t, adopted.Path, "show", result.Snapshot.Record.SnapshotSHA+":later.txt"); got != "uncommitted child work" {
		t.Fatalf("dirty result=%q", got)
	}
	if got := childSnapshotGit(t, adopted.Path, "ls-tree", "--name-only", result.Snapshot.Record.SnapshotSHA, "--", ".goobers"); got != "" {
		t.Fatal("captured private runtime")
	}
	childSnapshotWrite(t, adopted.Path, "later.txt", "changed after result")
	repeated, err := coordinator.CaptureResult(t.Context(), submission.Child, &childWorkspace, terminal)
	if err != nil || repeated.ResultRef != result.ResultRef {
		t.Fatalf("result recaptured: %+v %v", repeated, err)
	}
	storedResult, err := service.Queue.ChildResult(t.Context(), submission.Child.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := importChildCarrier(t.Context(), parent, *result.Snapshot, storedResult.Bundle); err != nil {
		t.Fatal(err)
	}
	if got := childSnapshotGit(t, parent, "diff", "--name-only", admission.ForkSHA, result.Snapshot.Record.SnapshotSHA); !strings.Contains(got, "main.txt") || !strings.Contains(got, "later.txt") {
		t.Fatalf("result delta lost commits or dirty work: %s", got)
	}
	if err := service.Queue.BeginDispatch(t.Context(), submission.Child.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	if err := service.Queue.SetChildState(t.Context(), submission.Child.Identity, triggerqueue.ChildStateUpdate{Expected: triggerqueue.ChildQueued, State: triggerqueue.ChildRunning}, submission.Child.AcceptedAt); err != nil {
		t.Fatal(err)
	}
	if err := service.Queue.SetChildState(t.Context(), submission.Child.Identity, triggerqueue.ChildStateUpdate{Expected: triggerqueue.ChildRunning, State: triggerqueue.ChildCompleted, ResultRef: result.ResultRef, WorkspaceRef: result.WorkspaceRef}, terminal.FinishedAt); err != nil {
		t.Fatal(err)
	}
	service.Now = func() time.Time { return terminal.FinishedAt.Add(time.Second) }
	request, err := service.RequestDisposition(t.Context(), authority.current.Origin, "child", "replace", result.ResultRef)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Plan) != 0 || !request.AppliedAt.IsZero() {
		t.Fatal("request applied live harness state")
	}
	if _, err := service.Submit(t.Context(), authority.current.Origin, SubmissionRequest{InvocationKey: "next", Source: []byte(validProposal)}); !errors.Is(err, triggerqueue.ErrChildSlotOccupied) {
		t.Fatal("request released slot before application", err)
	}
	if _, err := coordinator.ApplyDisposition(t.Context(), submission.Child, &yielded, service.now()); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(parent, "main.txt")); string(data) != "child progress" {
		t.Fatal("child committed result not applied to parent")
	}
	if data, _ := os.ReadFile(filepath.Join(parent, "later.txt")); string(data) != "uncommitted child work" {
		t.Fatal("child dirty result not applied to parent")
	}
	if data, _ := os.ReadFile(filepath.Join(parent, "auth/token")); string(data) != "secret credential" {
		t.Fatal("application changed excluded parent credential")
	}
	// A lost response after applied acknowledgement cannot recapture or reapply
	// child state over progress made by the resumed parent.
	childSnapshotWrite(t, parent, "later.txt", "new parent continuation work")
	if _, err := coordinator.ApplyDisposition(t.Context(), submission.Child, &yielded, service.now()); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(parent, "later.txt")); string(data) != "new parent continuation work" {
		t.Fatal("applied retry overwrote continued parent")
	}
	if _, err := service.Submit(t.Context(), authority.current.Origin, SubmissionRequest{InvocationKey: "next", Source: []byte(validProposal)}); err != nil {
		t.Fatal("applied disposition failed to release slot", err)
	}
	if _, err := submissionDB(t, databasePath).Exec(`DELETE FROM child_snapshots`); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Capture(t.Context(), submission.Child, yielded); !errors.Is(err, triggerqueue.ErrChildSnapshotUnavailable) {
		t.Fatalf("lost carrier was recaptured: %v", err)
	}
}
