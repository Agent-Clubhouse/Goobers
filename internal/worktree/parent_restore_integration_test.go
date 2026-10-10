//go:build integration

package worktree_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

type parentRestoreLog struct{}

func (parentRestoreLog) Append(journal.Event) error { return nil }

func TestIntegrationParentRestoreRecreatesManagedBranchAndPreservesRetry(t *testing.T) {
	testdep.Require(t, "git")
	source := t.TempDir()
	childWorkspaceGit(t, source, "init", "--initial-branch=main")
	writeParentRestoreFile(t, source, "base\n")
	childWorkspaceGit(t, source, "add", ".")
	childWorkspaceGit(t, source, "commit", "-m", "base")
	base := childWorkspaceGit(t, source, "rev-parse", "HEAD")
	writeParentRestoreFile(t, source, "committed\n")
	childWorkspaceGit(t, source, "commit", "-am", "ordinary stage commit")
	head := childWorkspaceGit(t, source, "rev-parse", "HEAD")
	writeParentRestoreFile(t, source, "staged\n")
	childWorkspaceGit(t, source, "add", "main.txt")
	writeParentRestoreFile(t, source, "dirty\n")
	key := (providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "repo"}).CanonicalKey()
	at := time.Date(2026, 10, 9, 1, 0, 0, 0, time.UTC)
	policy := recovery.SnapshotPolicy{}
	record, path, err := recovery.Retain(t.Context(), recovery.RetentionRequest{Repository: source, RepositoryKey: key, RunID: "parent", BaseRef: base, IdentityTime: at, RetainUntil: at.Add(time.Hour), InventoryRoot: t.TempDir(), CleanupRoots: []string{source}, MaxSnapshots: 1, MaxArchiveBytes: 1 << 20, ParentPolicy: &policy}, parentRestoreLog{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := worktree.NewManager(t.TempDir(), worktree.WithRemoteGitGate(func(context.Context, string) error {
		t.Fatal("hydrated parent restoration attempted remote access")
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	var mirror string
	if err := manager.WithRecoveryMirror(t.Context(), source, func(dir string) error {
		mirror = dir
		childWorkspaceGit(t, dir, "fetch", source, base)
		return recovery.ImportSnapshotBundle(t.Context(), dir, filepath.Join(filepath.Dir(path), recovery.BundleFileName), record, 1<<20)
	}); err != nil {
		t.Fatal(err)
	}
	opts := worktree.ParentRestoreOptions{RepoURL: source, RunID: "parent-restoration", OwnerRunID: "parent", Gaggle: "gaggle", Branch: "goobers/parent", BaseRef: base, HeadSHA: head, StartRef: base}
	wt, err := manager.CreateParentRestore(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := childWorkspaceGit(t, wt.Path, "rev-parse", "HEAD"); got != head {
		t.Fatal("restoration checked out cumulative base instead of original HEAD")
	}
	plan, err := recovery.PlanRetainedParentRestore(t.Context(), wt.Path, record, "restore-parent", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.ApplyChildApplication(t.Context(), wt.Path, plan); err != nil {
		t.Fatal(err)
	}
	held, err := wt.HoldForChild(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if held.StartRef != base {
		t.Fatal("restoration replaced the original custody anchor", held)
	}
	retry, err := manager.CreateParentRestore(t.Context(), opts)
	if err != nil || retry.Path != wt.Path {
		t.Fatal("identical restoration did not preserve managed custody", err)
	}
	if _, err := manager.AdoptHeldStage(t.Context(), source, held); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(retry.Path, "main.txt")); err != nil || string(data) != "dirty\n" || childWorkspaceGit(t, retry.Path, "show", ":main.txt") != "staged" {
		t.Fatal("restoration retry reset staged or dirty state", err)
	}
	wrong := opts
	wrong.OwnerRunID = "another-owner"
	if _, err := manager.CreateParentRestore(t.Context(), wrong); err == nil {
		t.Fatal("restoration borrowed another owner's workspace")
	}
	wrong = opts
	wrong.HeadSHA = base
	if _, err := manager.CreateParentRestore(t.Context(), wrong); err == nil {
		t.Fatal("restoration rewound an existing workspace")
	}
	// An absent physical checkout cannot be used to rewind a branch which
	// advanced after archive capture, either.
	wrong = opts
	wrong.RunID = "another-physical-restoration"
	wrong.HeadSHA = base
	if _, err := manager.CreateParentRestore(t.Context(), wrong); err == nil || childWorkspaceGit(t, mirror, "rev-parse", "refs/heads/"+opts.Branch) != head {
		t.Fatal("restoration rewound a surviving branch", err)
	}
	alias := opts
	alias.RunID, alias.Branch = "alias-restoration", "goobers/alias"
	childWorkspaceGit(t, mirror, "symbolic-ref", "refs/heads/"+alias.Branch, "refs/heads/"+opts.Branch)
	if _, err := manager.CreateParentRestore(t.Context(), alias); err == nil {
		t.Fatal("restoration accepted a branch alias")
	}
	verifiedBase := false
	if err := manager.SetCleanupGuard("check-archive-base", func(_ context.Context, target worktree.CleanupTarget) error {
		verifiedBase = target.BaseRef == base && target.StartRef == base
		if !verifiedBase {
			t.Fatal("restoration lost cumulative cleanup base", target)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := wt.ReleaseChildHold(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := wt.Remove(t.Context(), worktree.RemoveOptions{}); err != nil || !verifiedBase {
		t.Fatal("restored workspace cleanup bypassed cumulative archive base", err)
	}
}

func writeParentRestoreFile(t *testing.T, repo, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, "main.txt"), []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}
