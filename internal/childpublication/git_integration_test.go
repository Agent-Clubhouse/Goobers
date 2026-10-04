//go:build integration

package childpublication

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func publicationGitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	testdep.Require(t, "git")
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.com", "GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.com", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s: %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}
func publicationRepository(t *testing.T, target Target) Target {
	t.Helper()
	target.Workspace = t.TempDir()
	target.Remote = filepath.Join(t.TempDir(), "origin.git")
	publicationGitTest(t, target.Workspace, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(target.Workspace, "base.txt"), []byte("base"), 0600); err != nil {
		t.Fatal(err)
	}
	publicationGitTest(t, target.Workspace, "add", ".")
	publicationGitTest(t, target.Workspace, "commit", "-m", "base")
	publicationGitTest(t, target.Workspace, "init", "--bare", target.Remote)
	publicationGitTest(t, target.Workspace, "push", target.Remote, "main")
	fork, err := recovery.CaptureChildSnapshot(t.Context(), target.Workspace, target.Repository.CanonicalKey(), target.Identity.Child.ParentRunID, target.Child.AcceptedAt, target.Child.AcceptedAt.Add(24*time.Hour), recovery.SnapshotPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	target.Fork = fork
	publicationGitTest(t, target.Workspace, "checkout", "-b", "goobers/children/"+target.Child.RunID, fork.Record.SnapshotSHA)
	if err = os.WriteFile(filepath.Join(target.Workspace, "child.txt"), []byte("child work"), 0600); err != nil {
		t.Fatal(err)
	}
	return target
}

type lostPushGit struct {
	GitCommand
	creates int
}

func (g *lostPushGit) Create(ctx context.Context, workspace, remote, head, sha string) error {
	g.creates++
	if err := g.GitCommand.Create(ctx, workspace, remote, head, sha); err != nil {
		return err
	}
	return errors.New("lost push response")
}
func TestIntegrationChildPublicationPushReopensWithoutSecondEffect(t *testing.T) {
	q, target, path := publicationFixture(t)
	target = publicationRepository(t, target)
	git := &lostPushGit{GitCommand: GitCommand{AllowLocal: true}}
	p := Publisher{Queue: q, Git: git}
	before := publicationGitTest(t, target.Workspace, "rev-parse", "HEAD")
	index := publicationGitTest(t, target.Workspace, "write-tree")
	// Workspace-controlled hooks and URL rewriting must never enter transport.
	hook := filepath.Join(t.TempDir(), "hook-ran")
	hooks := filepath.Join(t.TempDir(), "hooks")
	if err := os.MkdirAll(hooks, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte("#!/bin/sh\ntouch '"+hook+"'\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	publicationGitTest(t, target.Workspace, "config", "core.hooksPath", hooks)
	publicationGitTest(t, target.Workspace, "config", "url./nonexistent/.insteadOf", target.Remote)
	if _, err := p.Push(t.Context(), target); err == nil {
		t.Fatal("lost response confirmed")
	}
	rec, err := q.ChildPublication(t.Context(), target.Child.Identity, "branch")
	if err != nil || rec.State != "effect_pending" {
		t.Fatal(rec, err)
	}
	// Reopen the real SQLite database through its known absolute path.
	if err = q.Close(); err != nil {
		t.Fatal(err)
	}
	q, err = triggerqueue.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = q.Close() }()
	p.Queue = q
	receipt, err := p.Push(t.Context(), target)
	if err != nil || receipt.Head != target.Head {
		t.Fatal(receipt, err)
	}
	if git.creates != 1 {
		t.Fatal("duplicate push", git.creates)
	}
	if _, err = os.Stat(hook); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("workspace hook executed", err)
	}
	publicationGitTest(t, target.Workspace, "merge-base", "--is-ancestor", target.Fork.Record.BaseSHA, receipt.SHA)
	if publicationGitTest(t, target.Workspace, "rev-parse", "HEAD") != before || publicationGitTest(t, target.Workspace, "write-tree") != index {
		t.Fatal("publication mutated managed custody")
	}
	if err = os.WriteFile(filepath.Join(target.Workspace, "child.txt"), []byte("new revision"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Push(t.Context(), target); !errors.Is(err, triggerqueue.ErrConflict) {
		t.Fatal("revision silently replaced first intent", err)
	}
}
func TestIntegrationChildPublicationRejectsExistingForeignBranch(t *testing.T) {
	q, target, _ := publicationFixture(t)
	target = publicationRepository(t, target)
	publicationGitTest(t, target.Workspace, "push", target.Remote, "HEAD:refs/heads/"+target.Head)
	p := Publisher{Queue: q, Git: GitCommand{AllowLocal: true}}
	if _, err := p.Push(t.Context(), target); err == nil {
		t.Fatal("foreign branch adopted")
	}
	if got := publicationGitTest(t, target.Workspace, "--git-dir="+target.Remote, "rev-parse", "refs/heads/"+target.Head); got != target.Fork.Record.SnapshotSHA {
		t.Fatal("foreign branch overwritten", got)
	}
}
