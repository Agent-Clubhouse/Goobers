//go:build integration

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
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
	if err := f.run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: "plan", Attempt: 1, Runner: map[string]any{"kind": runner.ChildWaitKind, "childWait": map[string]any{"version": 1, "parentRunId": f.env.RunID, "request": request, "policyAttempts": 0, "infrastructureFailures": 0}}}); err != nil {
		t.Fatal(err)
	}
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
