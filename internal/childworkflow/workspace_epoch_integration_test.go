//go:build integration

package childworkflow

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationHumanChildEpochForkPreservesSealedContribution(t *testing.T) {
	testdep.Require(t, "git")
	service, authority, dbPath := submissionFixture(t)
	accepted, err := service.Submit(t.Context(), authority.current.Origin, SubmissionRequest{InvocationKey: "epoch", Source: []byte(validProposal)})
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	childSnapshotGit(t, parent, "init", "--initial-branch=main")
	childSnapshotWrite(t, parent, "base.txt", "parent base")
	childSnapshotGit(t, parent, "add", ".")
	childSnapshotGit(t, parent, "commit", "-m", "base")
	manager, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.WorkingCopy(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	c := WorkspaceCoordinator{Queue: service.Queue, Worktrees: manager}
	key := (providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}).CanonicalKey()
	yielded := YieldedWorkspace{Path: parent, RepoURL: parent, RepositoryKey: key, Policy: recovery.SnapshotPolicy{ExcludedPaths: []string{"private"}}}
	if err := c.Capture(t.Context(), accepted.Child, yielded); err != nil {
		t.Fatal(err)
	}
	initial, err := c.Prepare(t.Context(), accepted.Child, parent)
	if err != nil {
		t.Fatal(err)
	}
	owned, err := manager.AdoptChildFromSnapshot(t.Context(), worktree.ChildOptions{RepoURL: parent, RunID: initial.WorkspaceID, OwnerRunID: accepted.Child.RunID, Gaggle: accepted.Child.Identity.Gaggle, SnapshotSHA: initial.ForkSHA})
	if err != nil {
		t.Fatal(err)
	}
	childSnapshotWrite(t, owned.Path, "committed.txt", "source commit")
	childSnapshotGit(t, owned.Path, "add", "committed.txt")
	childSnapshotGit(t, owned.Path, "commit", "-m", "source progress")
	childSnapshotWrite(t, owned.Path, "dirty.txt", "source uncommitted progress")
	childSnapshotWrite(t, owned.Path, "private/token", "excluded")
	yielded.Path = owned.Path
	input := TerminalResultInput{State: triggerqueue.ChildFailed, FinishedAt: accepted.Child.AcceptedAt.Add(time.Second), Summary: "needs human guidance"}
	first, err := c.CaptureResult(t.Context(), accepted.Child, &yielded, input)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Queue.SetChildState(t.Context(), accepted.Child.Identity, triggerqueue.ChildStateUpdate{Expected: triggerqueue.ChildQueued, State: triggerqueue.ChildFailed, ResultRef: first.ResultRef, WorkspaceRef: first.WorkspaceRef}, input.FinishedAt); err != nil {
		t.Fatal(err)
	}
	source, _ := service.Queue.GetChild(t.Context(), accepted.Child.Identity)
	prior, _ := service.Queue.ChildResult(t.Context(), source.Identity)
	current := restartResultChild(t, service.Queue, source, first)
	childSnapshotWrite(t, owned.Path, "dirty.txt", "later source edits are not a restart source")
	if err := service.Queue.Close(); err != nil {
		t.Fatal(err)
	}
	service.Queue, err = triggerqueue.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	c.Queue = service.Queue
	if _, err := c.Prepare(t.Context(), current, parent); err == nil {
		t.Fatal("original fork path admitted new epoch")
	}
	admission, err := c.PrepareExecution(t.Context(), current, parent)
	if err != nil {
		t.Fatal(err)
	}
	if admission.WorkspaceID == initial.WorkspaceID || admission.ForkSHA != first.WorkspaceRef {
		t.Fatal("epoch did not select distinct sealed source", admission)
	}
	epochWorkspace, err := manager.AdoptChildFromSnapshot(t.Context(), worktree.ChildOptions{RepoURL: parent, RunID: admission.WorkspaceID, OwnerRunID: current.ActiveRunID(), Gaggle: current.Identity.Gaggle, SnapshotSHA: admission.ForkSHA})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"committed.txt": "source commit", "dirty.txt": "source uncommitted progress"} {
		got, err := os.ReadFile(filepath.Join(epochWorkspace.Path, name))
		if err != nil || string(got) != want {
			t.Fatalf("%s=%q: %v", name, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(epochWorkspace.Path, "private/token")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("excluded custody escaped", err)
	}
	childSnapshotWrite(t, epochWorkspace.Path, "human.txt", "new epoch progress")
	if _, err := c.PrepareExecution(t.Context(), current, parent); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(epochWorkspace.Path, "human.txt")); err != nil || string(got) != "new epoch progress" {
		t.Fatal("replay reset human progress", err)
	}
	yielded.Path = epochWorkspace.Path
	input.State, input.Summary, input.FinishedAt = triggerqueue.ChildCompleted, "resolved", current.UpdatedAt.Add(time.Second)
	second, err := c.CaptureResult(t.Context(), current, &yielded, input)
	if err != nil {
		t.Fatal(err)
	}
	if second.Snapshot.Record.BaseSHA != initial.ForkSHA || second.Snapshot.Record.RunID != current.ActiveRunID() {
		t.Fatal("result lost original parent prerequisite or epoch identity", second)
	}
	for _, name := range []string{"committed.txt", "dirty.txt", "human.txt"} {
		if got := childSnapshotGit(t, epochWorkspace.Path, "show", second.WorkspaceRef+":"+name); got == "" {
			t.Fatal("result lost contribution", name)
		}
	}
	retained, err := service.Queue.ChildExecutionResult(t.Context(), current.Identity, source.RunID)
	if err != nil || !bytes.Equal(retained.Receipt, prior.Receipt) || !bytes.Equal(retained.Bundle, prior.Bundle) {
		t.Fatal("source result changed", err)
	}
	if _, err := os.Stat(filepath.Join(parent, "human.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("restart modified parent", err)
	}
	if err := service.Queue.FenceChildParent(t.Context(), current.Identity.ChildParent, "human cancellation", current.UpdatedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PrepareExecution(t.Context(), current, parent); err == nil {
		t.Fatal("cancelled parent admitted epoch workspace")
	}
}
