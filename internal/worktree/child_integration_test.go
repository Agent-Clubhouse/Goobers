//go:build integration

package worktree_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationChildWorktreeForkPreservesParentAndRetries(t *testing.T) {
	testdep.Require(t, "git")
	ctx := context.Background()
	source := t.TempDir()
	childWorkspaceGit(t, source, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(source, "main.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	childWorkspaceGit(t, source, "add", ".")
	childWorkspaceGit(t, source, "commit", "-m", "base")
	allowRemote := true
	manager, err := worktree.NewManager(t.TempDir(), worktree.WithRemoteGitGate(func(context.Context, string) error {
		if !allowRemote {
			t.Fatal("child fork attempted forge access")
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.WorkingCopy(ctx, source); err != nil {
		t.Fatal(err)
	}
	allowRemote = false
	head := childWorkspaceGit(t, source, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(source, "main.txt"), []byte("parent dirty\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "new.bin"), []byte{0, 255, 1}, 0600); err != nil {
		t.Fatal(err)
	}
	key := (providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}).CanonicalKey()
	at := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	snapshot, err := recovery.CaptureChildSnapshot(ctx, source, key, "parent", at, at.Add(time.Hour), recovery.SnapshotPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	snapshot, err = recovery.WriteChildSnapshotBundle(ctx, source, snapshot, &archive, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "snapshot.bundle")
	if err := os.WriteFile(archivePath, archive.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := manager.WithRecoveryMirror(ctx, source, func(mirror string) error {
		return recovery.ImportChildSnapshot(ctx, mirror, archivePath, snapshot, 1<<20)
	}); err != nil {
		t.Fatal(err)
	}
	opts := worktree.ChildOptions{RepoURL: source, RunID: "child-stage", OwnerRunID: "child-run", Gaggle: "web", SnapshotSHA: snapshot.Record.SnapshotSHA}
	child, err := manager.CreateChildFromSnapshot(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if child.Path == source {
		t.Fatal("child shares parent checkout")
	}
	if got, _ := os.ReadFile(filepath.Join(child.Path, "main.txt")); string(got) != "parent dirty\n" {
		t.Fatal("child lost uncommitted parent edit")
	}
	if got, _ := os.ReadFile(filepath.Join(child.Path, "new.bin")); !bytes.Equal(got, []byte{0, 255, 1}) {
		t.Fatal("child lost untracked binary")
	}
	if err := os.WriteFile(filepath.Join(child.Path, "main.txt"), []byte("child progressed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	retry, err := manager.CreateChildFromSnapshot(ctx, opts)
	if err != nil || retry.Path != child.Path {
		t.Fatalf("retry=%+v %v", retry, err)
	}
	if got, _ := os.ReadFile(filepath.Join(child.Path, "main.txt")); string(got) != "child progressed\n" {
		t.Fatal("retry reset child work")
	}
	if got, _ := os.ReadFile(filepath.Join(source, "main.txt")); string(got) != "parent dirty\n" {
		t.Fatal("child changed parent")
	}
	if got := childWorkspaceGit(t, source, "rev-parse", "HEAD"); got != head {
		t.Fatal("child changed parent HEAD")
	}
	opts.SnapshotSHA = head
	if _, err := manager.CreateChildFromSnapshot(ctx, opts); err == nil {
		t.Fatal("changed fork identity accepted on same workspace")
	}
	if got, _ := os.ReadFile(filepath.Join(child.Path, "main.txt")); string(got) != "child progressed\n" {
		t.Fatal("conflicting retry changed child work")
	}
}

func childWorkspaceGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	base := []string{"-C", dir, "-c", "user.name=Child Workspace Test", "-c", "user.email=child-test@goobers.invalid", "-c", "commit.gpgsign=false"}
	command := exec.Command("git", append(base, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}
