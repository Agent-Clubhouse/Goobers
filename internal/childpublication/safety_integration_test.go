//go:build integration

package childpublication

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func unsafePublicationHistory(t *testing.T, target Target, scenario string) Target {
	t.Helper()
	path, contents := ".goobers/debug.log", "private diagnostic output"
	if scenario == "renamed credential log" {
		path, contents = "debug.log", "GH_TOKEN="+strings.Repeat("x", 40)
	}
	absolute := filepath.Join(target.Workspace, path)
	if err := os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absolute, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	publicationGitTest(t, target.Workspace, "add", "--force", path)
	publicationGitTest(t, target.Workspace, "commit", "-m", "unsafe intermediate commit")
	if scenario == "renamed credential log" {
		publicationGitTest(t, target.Workspace, "mv", path, "notes.txt")
	} else {
		publicationGitTest(t, target.Workspace, "rm", path)
	}
	publicationGitTest(t, target.Workspace, "commit", "-m", "later tree change")
	if scenario == "unsafe parent fork history" {
		publicationGitTest(t, target.Workspace, "add", "child.txt")
		publicationGitTest(t, target.Workspace, "commit", "-m", "parent work before child fork")
		fork, err := recovery.CaptureChildSnapshot(t.Context(), target.Workspace, target.Repository.CanonicalKey(), target.Identity.Child.ParentRunID, target.Child.AcceptedAt, target.Child.AcceptedAt.Add(24*time.Hour), recovery.SnapshotPolicy{})
		if err != nil {
			t.Fatal(err)
		}
		target.Fork = fork
		publicationGitTest(t, target.Workspace, "checkout", "--detach", fork.Record.SnapshotSHA)
	}
	return target
}

func TestIntegrationChildPublicationBlocksUnsafeOutgoingHistory(t *testing.T) {
	testdep.Require(t, "git")
	for _, scenario := range []string{"introduced then deleted artifact", "renamed credential log", "unsafe parent fork history"} {
		t.Run(scenario, func(t *testing.T) {
			q, target, _ := publicationFixture(t)
			target = unsafePublicationHistory(t, publicationRepository(t, target), scenario)
			before := publicationGitTest(t, target.Workspace, "rev-parse", "HEAD")
			index := publicationGitTest(t, target.Workspace, "write-tree")
			p := Publisher{Queue: q, Git: GitCommand{AllowLocal: true}}
			if _, err := p.Push(t.Context(), target); err == nil || !strings.Contains(err.Error(), "publication safety") || strings.Contains(err.Error(), strings.Repeat("x", 40)) {
				t.Fatal("unsafe outgoing history accepted or diagnostics leaked", err)
			}
			if _, err := q.ChildPublication(t.Context(), target.Child.Identity, "branch"); !errors.Is(err, triggerqueue.ErrChildPublicationPending) {
				t.Fatal("blocked publication retained an intent", err)
			}
			if sha, err := p.Git.Head(t.Context(), target.Workspace, target.Remote, target.Head); err != nil || sha != "" {
				t.Fatal("unsafe branch reached remote", err)
			}
			if publicationGitTest(t, target.Workspace, "rev-parse", "HEAD") != before || publicationGitTest(t, target.Workspace, "write-tree") != index {
				t.Fatal("blocked publication changed workspace custody")
			}
		})
	}
}

func TestIntegrationChildPublicationPRRechecksLegacyUnsafeHistory(t *testing.T) {
	testdep.Require(t, "git")
	for _, scenario := range []string{"introduced then deleted artifact", "renamed credential log"} {
		t.Run(scenario, func(t *testing.T) {
			q, target, _ := publicationFixture(t)
			target = unsafePublicationHistory(t, publicationRepository(t, target), scenario)
			sha := publicationGitTest(t, target.Workspace, "rev-parse", "HEAD")
			// Model a branch pushed and confirmed before the guard was installed.
			publicationGitTest(t, target.Workspace, "push", target.Remote, sha+":refs/heads/"+target.Head)
			confirmPublicationCommit(t, q, target, sha)
			fake := &publicationHTTP{t: t, target: target}
			p := Publisher{Queue: q, Git: GitCommand{AllowLocal: true}, PRs: providers.NewGitHubProvider("token", providers.WithHTTPClient(fake), providers.WithMaxTransientRetries(0))}
			if _, err := p.OpenPR(t.Context(), target, "child title", "body", true); err == nil || !strings.Contains(err.Error(), "publication safety") {
				t.Fatal("legacy unsafe branch admitted", err)
			}
			if fake.creates != 0 || fake.patches != 0 {
				t.Fatal("legacy unsafe branch reached provider writes")
			}
			if _, err := q.ChildPublication(t.Context(), target.Child.Identity, "pr"); !errors.Is(err, triggerqueue.ErrChildPublicationPending) {
				t.Fatal("legacy unsafe branch prepared PR intent", err)
			}
		})
	}
}

func TestIntegrationChildPublicationSafetyReadsSourceBeyondTransportLimit(t *testing.T) {
	testdep.Require(t, "git")
	q, target, _ := publicationFixture(t)
	target = publicationRepository(t, target)
	if err := os.WriteFile(filepath.Join(target.Workspace, "large.txt"), []byte(strings.Repeat("ordinary source\n", 2000)), 0600); err != nil {
		t.Fatal(err)
	}
	p := Publisher{Queue: q, Git: GitCommand{AllowLocal: true}}
	receipt, err := p.Push(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	if sha, err := p.Git.Head(t.Context(), target.Workspace, target.Remote, target.Head); err != nil || sha != receipt.SHA {
		t.Fatal("safe exact snapshot not published", err)
	}
	// Later workspace history cannot substitute for the confirmed snapshot.
	unsafePublicationHistory(t, target, "introduced then deleted artifact")
	fake := &publicationHTTP{t: t, target: target}
	p.PRs = providers.NewGitHubProvider("token", providers.WithHTTPClient(fake), providers.WithMaxTransientRetries(0))
	if _, err := p.OpenPR(t.Context(), target, "child title", "body", true); err != nil || fake.creates != 1 {
		t.Fatal("PR did not inspect the exact confirmed snapshot", err)
	}
	secret := "ghp_" + strings.Repeat("z", 36)
	if _, err := publicationReadGit(t.Context(), target.Workspace, "cat-file", "blob", secret); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatal("local Git error exposed raw diagnostics", err)
	}
}
