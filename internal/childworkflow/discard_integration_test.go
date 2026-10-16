//go:build integration

package childworkflow

import (
	"bytes"
	"encoding/json"
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

func TestIntegrationDiscardReturnedWorkspaceWithoutParentMutationAuthority(t *testing.T) {
	testdep.Require(t, "git")
	service, authority, _ := submissionFixture(t)
	submission, err := service.Submit(t.Context(), authority.current.Origin, SubmissionRequest{InvocationKey: "discard", Source: []byte(validProposal)})
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	childSnapshotGit(t, parent, "init", "--initial-branch=main")
	childSnapshotWrite(t, parent, "work.txt", "original")
	childSnapshotGit(t, parent, "add", ".")
	childSnapshotGit(t, parent, "commit", "-m", "original")
	manager, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manager.WorkingCopy(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	coordinator := WorkspaceCoordinator{Queue: service.Queue, Worktrees: manager}
	yielded := YieldedWorkspace{Path: parent, RepoURL: parent, RepositoryKey: (providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}).CanonicalKey()}
	if err = coordinator.Capture(t.Context(), submission.Child, yielded); err != nil {
		t.Fatal(err)
	}
	fork, err := coordinator.Prepare(t.Context(), submission.Child, parent)
	if err != nil {
		t.Fatal(err)
	}
	child, err := manager.AdoptChildFromSnapshot(t.Context(), worktree.ChildOptions{RepoURL: parent, RunID: fork.WorkspaceID, OwnerRunID: submission.Child.RunID, Gaggle: submission.Child.Identity.Gaggle, SnapshotSHA: fork.ForkSHA})
	if err != nil {
		t.Fatal(err)
	}
	childSnapshotWrite(t, child.Path, "work.txt", "returned child edit")
	returned := yielded
	returned.Path = child.Path
	finished := submission.Child.AcceptedAt.Add(time.Minute)
	result, err := coordinator.CaptureResult(t.Context(), submission.Child, &returned, TerminalResultInput{State: triggerqueue.ChildFailed, FinishedAt: finished})
	if err != nil {
		t.Fatal(err)
	}
	if err = service.Queue.SetChildState(t.Context(), submission.Child.Identity, triggerqueue.ChildStateUpdate{Expected: triggerqueue.ChildQueued, State: triggerqueue.ChildFailed, ResultRef: result.ResultRef, WorkspaceRef: result.WorkspaceRef}, finished); err != nil {
		t.Fatal(err)
	}
	service.Now = func() time.Time { return finished.Add(time.Second) }
	authority.current.Admission.WorkspaceMutationDenied = true
	if _, err = service.RequestDisposition(t.Context(), authority.current.Origin, "discard", "discard", result.ResultRef); err != nil {
		t.Fatal(err)
	}
	childSnapshotWrite(t, parent, "work.txt", "later parent edits")
	childSnapshotGit(t, parent, "add", "work.txt")
	beforeIndex, err := os.ReadFile(filepath.Join(parent, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	beforeHead := childSnapshotGit(t, parent, "rev-parse", "HEAD")
	// Discard verifies retained result custody, but the host need not give it
	// a mutable checkout path or the previous snapshot exclusion policy.
	readonly := YieldedWorkspace{RepoURL: parent, Policy: recovery.SnapshotPolicy{ExcludedPaths: []string{"new-private-path"}}}
	if _, err = coordinator.ApplyDisposition(t.Context(), submission.Child, &readonly, service.now()); err != nil {
		t.Fatal(err)
	}
	disposition, err := service.Queue.ChildDisposition(t.Context(), submission.Child.Identity)
	if err != nil {
		t.Fatal(err)
	}
	var intent childDispositionIntent
	if json.Unmarshal(disposition.Plan, &intent) != nil || intent.Action != "discard" || intent.Application != nil || disposition.AppliedAt.IsZero() {
		t.Fatal("discard did not settle without a filesystem plan", disposition)
	}
	updated, err := service.Queue.GetChild(t.Context(), submission.Child.Identity)
	if err != nil || updated.AcknowledgedAt.IsZero() {
		t.Fatal("discard failed to release retained child", updated, err)
	}
	afterIndex, err := os.ReadFile(filepath.Join(parent, ".git", "index"))
	if err != nil || !bytes.Equal(beforeIndex, afterIndex) || childSnapshotGit(t, parent, "rev-parse", "HEAD") != beforeHead {
		t.Fatal("discard changed parent index or commit", err)
	}
	if content, err := os.ReadFile(filepath.Join(parent, "work.txt")); err != nil || string(content) != "later parent edits" {
		t.Fatal("discard changed parent work", string(content), err)
	}
}
