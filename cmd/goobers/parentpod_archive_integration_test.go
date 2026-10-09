//go:build integration

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/worktree"
)

type parentArchiveLogFunc func(journal.Event) error

func (f parentArchiveLogFunc) Append(event journal.Event) error { return f(event) }

func verifyLatestParentCleanupArchive(t *testing.T, reader *journal.Reader, repo, key string, custody worktree.StageCustody) {
	t.Helper()
	target := worktree.CleanupTarget{Path: repo, WorktreeID: custody.WorkspaceID, OwnerRunID: custody.OwnerRunID, RepositoryDigest: custody.RepositoryDigest, StartRef: custody.StartRef, BaseRef: custody.StartRef, RetainOnCleanup: true}
	policy, err := parentCleanupPolicy(t.Context(), reader, target, key)
	if err != nil || policy == nil {
		t.Fatal("cleanup did not resolve the verified parent's policy", err)
	}
	wrong := target
	wrong.RepositoryDigest = journal.Digest([]byte("different repository"))
	if _, err := parentCleanupPolicy(t.Context(), reader, wrong, key); err == nil {
		t.Fatal("cleanup accepted a substituted repository")
	}
	// Ordinary stages can advance the branch after the pod's last return.
	// Archive the live checkout, not that older retained output artifact.
	recoveryCLIGit(t, repo, "commit", "-m", "ordinary stage committed after worker")
	head := recoveryCLIGit(t, repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "source.txt"), []byte("ordinary staged\n"), 0600); err != nil {
		t.Fatal(err)
	}
	recoveryCLIGit(t, repo, "add", "source.txt")
	if err := os.WriteFile(filepath.Join(repo, "source.txt"), []byte("ordinary dirty\n"), 0600); err != nil {
		t.Fatal(err)
	}
	id, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	request := recovery.RetentionRequest{Repository: repo, RepositoryKey: key, RunID: id.RunID, BaseRef: custody.StartRef, IdentityTime: id.StartedAt, RetainUntil: id.StartedAt.Add(24 * time.Hour), InventoryRoot: t.TempDir(), CleanupRoots: []string{repo}, MaxSnapshots: 1, MaxArchiveBytes: 1 << 20, ParentPolicy: policy}
	acknowledged := false
	record, path, err := recovery.Retain(t.Context(), request, parentArchiveLogFunc(func(event journal.Event) error {
		acknowledged = event.RunID == id.RunID && event.Runner["recoveryCapture"] == true
		return nil
	}))
	if err != nil || path == "" || !acknowledged {
		t.Fatal("parent cleanup archive not acknowledged", err)
	}
	state, err := recovery.ReadRetainedParentState(t.Context(), repo, record)
	if err != nil || state.HeadSHA != head {
		t.Fatal("archive lost subsequent ordinary commit", state, err)
	}
	if recoveryCLIGit(t, repo, "show", state.IndexSHA+":source.txt") != "ordinary staged" || recoveryCLIGit(t, repo, "show", record.SnapshotSHA+":source.txt") != "ordinary dirty" {
		t.Fatal("archive reused stale worker output")
	}
}
