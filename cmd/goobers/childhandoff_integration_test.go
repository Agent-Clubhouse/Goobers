//go:build integration

package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationDaemonChildHandoffCapturesOnceWithoutMutatingParent(t *testing.T) {
	testdep.Require(t, "git")
	parent := t.TempDir()
	f := newHandoffDaemonFixtureConfig(t, func(cfg *instance.Config) {
		cfg.Webhook.Secret = instance.TokenRef{File: filepath.Join(parent, "provider-auth.txt")}
	})
	recoveryCLIGit(t, parent, "init", "--initial-branch=main")
	write := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(parent, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("source.txt", "base\n")
	recoveryCLIGit(t, parent, "add", ".")
	recoveryCLIGit(t, parent, "commit", "-m", "base")
	write("source.txt", "staged\n")
	recoveryCLIGit(t, parent, "add", ".")
	write("source.txt", "working\n")
	write("new.txt", "untracked\n")
	write("provider-auth.txt", "configured credential fixture\n")
	before := recoveryCLIGit(t, parent, "status", "--porcelain=v1")
	index := recoveryCLIGit(t, parent, "write-tree")
	head := recoveryCLIGit(t, parent, "rev-parse", "HEAD")
	f.host.repoCloneURL = func(apiv1.RepoRef) (string, error) { return parent, nil }
	request, err := f.host.Await(t.Context(), f.env)
	if err != nil {
		t.Fatal(err)
	}
	recordDaemonHandoffWait(t, f, request)
	custody := runner.ChildWorkspaceCustody{Path: parent, RepoRef: f.host.project}
	if err := f.host.Yield(t.Context(), request, custody); err != nil {
		t.Fatal(err)
	}
	stored, err := f.queue.ChildSnapshot(t.Context(), f.child.Identity)
	if err != nil || len(stored.Bundle) == 0 {
		t.Fatal("daemon did not persist replay custody", stored, err)
	}
	coordinator := childworkflow.WorkspaceCoordinator{Queue: f.queue}
	fork, err := coordinator.RetainedFork(t.Context(), f.child, parent)
	if err != nil || !slices.Contains(fork.Policy.ExcludedPaths, "provider-auth.txt") {
		t.Fatal("configured credential path missing from snapshot policy", fork.Policy, err)
	}
	if strings.Contains(recoveryCLIGit(t, parent, "ls-tree", "-r", "--name-only", fork.Record.SnapshotSHA), "provider-auth.txt") {
		t.Fatal("credential file entered child snapshot")
	}
	if recoveryCLIGit(t, parent, "status", "--porcelain=v1") != before || recoveryCLIGit(t, parent, "write-tree") != index || recoveryCLIGit(t, parent, "rev-parse", "HEAD") != head {
		t.Fatal("capturing changed parent Git custody")
	}
	write("source.txt", "newer unrelated parent work\n")
	if err := f.host.Yield(t.Context(), request, custody); err != nil {
		t.Fatal(err)
	}
	replayed, err := f.queue.ChildSnapshot(t.Context(), f.child.Identity)
	if err != nil || replayed.ReceiptDigest != stored.ReceiptDigest || replayed.BundleDigest != stored.BundleDigest {
		t.Fatal("retry recaptured the mutable parent", err)
	}
	data, err := os.ReadFile(filepath.Join(parent, "source.txt"))
	if err != nil || string(data) != "newer unrelated parent work\n" {
		t.Fatal("retry altered parent files", string(data), err)
	}
}

func TestIntegrationDaemonDispositionUsesRetainedCredentialPolicy(t *testing.T) {
	testdep.Require(t, "git")
	parent := t.TempDir()
	f := newHandoffDaemonFixtureConfig(t, func(cfg *instance.Config) {
		cfg.Webhook.Secret = instance.TokenRef{File: filepath.Join(parent, "provider-auth.txt")}
	})
	recoveryCLIGit(t, parent, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(parent, "source.txt"), []byte("base"), 0600); err != nil {
		t.Fatal(err)
	}
	recoveryCLIGit(t, parent, "add", ".")
	recoveryCLIGit(t, parent, "commit", "-m", "base")
	f.host.repoCloneURL = func(apiv1.RepoRef) (string, error) { return parent, nil }
	request, err := f.host.Await(t.Context(), f.env)
	if err != nil {
		t.Fatal(err)
	}
	recordDaemonHandoffWait(t, f, request)
	custody := runner.ChildWorkspaceCustody{Path: parent, RepoRef: f.host.project}
	if err := f.host.Yield(t.Context(), request, custody); err != nil {
		t.Fatal(err)
	}
	coordinator := childworkflow.WorkspaceCoordinator{Queue: f.queue}
	fork, err := coordinator.RetainedFork(t.Context(), f.child, parent)
	if err != nil {
		t.Fatal(err)
	}
	childWorkspace := t.TempDir()
	recoveryCLIGit(t, parent, "worktree", "add", "--detach", childWorkspace, fork.Record.SnapshotSHA)
	result, err := coordinator.CaptureResult(t.Context(), f.child, &childworkflow.YieldedWorkspace{Path: childWorkspace, RepoURL: parent, RepositoryKey: fork.Record.RepositoryKey, Policy: fork.Policy}, childworkflow.TerminalResultInput{State: triggerqueue.ChildCompleted, FinishedAt: time.Now(), Summary: "done"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.queue.BeginDispatch(t.Context(), f.child.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	if err := f.queue.SetChildState(t.Context(), f.child.Identity, triggerqueue.ChildStateUpdate{Expected: triggerqueue.ChildQueued, State: triggerqueue.ChildRunning}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := f.queue.SetChildState(t.Context(), f.child.Identity, triggerqueue.ChildStateUpdate{Expected: triggerqueue.ChildRunning, State: triggerqueue.ChildCompleted, ResultRef: result.ResultRef}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := f.run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: "plan", Attempt: 1, Runner: map[string]any{"kind": runner.ChildContinuedKind, "requestId": request.RequestID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.queue.RequestChildDisposition(t.Context(), triggerqueue.ChildDispositionRequest{Identity: f.child.Identity, Action: "discard", ResultRef: result.ResultRef, Authority: f.grant}, time.Now()); err != nil {
		t.Fatal(err)
	}
	// The policy mapper uses retained custody even if the currently configured
	// credential paths differ. Full authority still independently checks config.
	original := f.service.config
	changed := *original
	changed.Webhook.Secret = instance.TokenRef{}
	f.service.config = &changed
	yielded, err := f.host.yieldedWorkspace(t.Context(), f.service, &coordinator, f.child, "discard", custody, parent)
	if err != nil || !slices.Equal(yielded.Policy.ExcludedPaths, fork.Policy.ExcludedPaths) {
		t.Fatal("disposition recomputed changed policy", yielded.Policy, err)
	}
	changed.Webhook.Secret = instance.TokenRef{File: filepath.Join(parent, "new-private.txt")}
	if _, err := f.host.yieldedWorkspace(t.Context(), f.service, &coordinator, f.child, "merge", custody, parent); !errors.Is(err, childworkflow.ErrAuthorityChanged) {
		t.Fatalf("new credential exclusion did not fence mutation: %v", err)
	}
	if _, err := f.host.yieldedWorkspace(t.Context(), f.service, &coordinator, f.child, "discard", custody, parent); err != nil {
		t.Fatalf("new credential exclusion prevented nonmutating discard: %v", err)
	}
	f.service.config = original
	disposition, err := f.host.Await(t.Context(), f.env)
	if err != nil {
		t.Fatal(err)
	}
	recordDaemonHandoffWait(t, f, disposition)
	if err := f.host.Yield(t.Context(), disposition, custody); err != nil {
		t.Fatal("disposition recomputed changed policy", err)
	}
	current, err := f.queue.GetChild(t.Context(), f.child.Identity)
	if err != nil || current.AcknowledgedAt.IsZero() {
		t.Fatal(current, err)
	}
}

func TestIntegrationDaemonMergeConflictCanBeRevisedToDiscard(t *testing.T) {
	testdep.Require(t, "git")
	f := newHandoffDaemonFixture(t)
	parent := t.TempDir()
	recoveryCLIGit(t, parent, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(parent, "source.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	recoveryCLIGit(t, parent, "add", ".")
	recoveryCLIGit(t, parent, "commit", "-m", "base")
	f.host.repoCloneURL = func(apiv1.RepoRef) (string, error) { return parent, nil }
	wait, err := f.host.Await(t.Context(), f.env)
	if err != nil {
		t.Fatal(err)
	}
	recordDaemonHandoffWait(t, f, wait)
	custody := runner.ChildWorkspaceCustody{Path: parent, RepoRef: f.host.project}
	if err := f.host.Yield(t.Context(), wait, custody); err != nil {
		t.Fatal(err)
	}
	coordinator := childworkflow.WorkspaceCoordinator{Queue: f.queue}
	fork, err := coordinator.RetainedFork(t.Context(), f.child, parent)
	if err != nil {
		t.Fatal(err)
	}
	childPath := t.TempDir()
	recoveryCLIGit(t, parent, "worktree", "add", "--detach", childPath, fork.Record.SnapshotSHA)
	if err := os.WriteFile(filepath.Join(childPath, "source.txt"), []byte("child change\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.CaptureResult(t.Context(), f.child, &childworkflow.YieldedWorkspace{Path: childPath, RepoURL: parent, RepositoryKey: fork.Record.RepositoryKey, Policy: fork.Policy}, childworkflow.TerminalResultInput{State: triggerqueue.ChildFailed, FinishedAt: time.Now(), Summary: "partial child result"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.queue.SetChildState(t.Context(), f.child.Identity, triggerqueue.ChildStateUpdate{Expected: triggerqueue.ChildQueued, State: triggerqueue.ChildFailed, ResultRef: result.ResultRef}, time.Now()); err != nil {
		t.Fatal(err)
	}
	clearWait := func(request runner.ChildHandoffRequest) {
		t.Helper()
		if err := f.run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: "plan", Attempt: 1, Runner: map[string]any{"kind": runner.ChildContinuedKind, "requestId": request.RequestID}}); err != nil {
			t.Fatal(err)
		}
	}
	clearWait(wait)
	if err := os.WriteFile(filepath.Join(parent, "source.txt"), []byte("parent change\n"), 0600); err != nil {
		t.Fatal(err)
	}
	merge, err := f.queue.RequestChildDisposition(t.Context(), triggerqueue.ChildDispositionRequest{Identity: f.child.Identity, Action: "merge", ResultRef: result.ResultRef, Authority: f.grant}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	request, err := f.host.Await(t.Context(), f.env)
	if err != nil {
		t.Fatal(err)
	}
	recordDaemonHandoffWait(t, f, request)
	var blocked *runner.ChildDispositionWaitError
	if err := f.host.Yield(t.Context(), request, custody); !errors.As(err, &blocked) || blocked.Reconcile {
		t.Fatal("merge conflict did not permit safe decision revision", err)
	}
	after, err := f.queue.ChildDisposition(t.Context(), f.child.Identity)
	if err != nil || len(after.Plan) != 0 {
		t.Fatal("conflicting merge published a live application", after, err)
	}
	data, err := os.ReadFile(filepath.Join(parent, "source.txt"))
	if err != nil || string(data) != "parent change\n" {
		t.Fatal("conflicting preparation changed parent", string(data), err)
	}
	clearWait(request)
	if _, err := f.queue.RequestChildDisposition(t.Context(), triggerqueue.ChildDispositionRequest{Identity: f.child.Identity, Action: "discard", ResultRef: result.ResultRef, Authority: f.grant, ExpectedRequestDigest: merge.RequestDigest()}, time.Now()); err != nil {
		t.Fatal(err)
	}
	discard, err := f.host.Await(t.Context(), f.env)
	if err != nil || discard.RequestID == request.RequestID {
		t.Fatal(discard, err)
	}
	recordDaemonHandoffWait(t, f, discard)
	if err := f.host.Yield(t.Context(), discard, custody); err != nil {
		t.Fatal(err)
	}
	child, err := f.queue.GetChild(t.Context(), f.child.Identity)
	if err != nil || child.AcknowledgedAt.IsZero() {
		t.Fatal("discard did not release custody", child, err)
	}
	data, err = os.ReadFile(filepath.Join(parent, "source.txt"))
	if err != nil || string(data) != "parent change\n" {
		t.Fatal("discard changed parent", string(data), err)
	}
}
