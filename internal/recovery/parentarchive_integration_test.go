//go:build integration

package recovery

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationParentRetentionKeepsLatestCommitIndexAndWorkingState(t *testing.T) {
	testdep.Require(t, "git")
	repo, key, at := childSnapshotFixture(t)
	childSnapshotWrite(t, repo, "private/committed", "original repository value\n")
	recoveryTestGit(t, repo, "add", "private/committed")
	recoveryTestGit(t, repo, "commit", "-m", "original excluded source path")
	base := recoveryTestGit(t, repo, "rev-parse", "HEAD")
	childSnapshotWrite(t, repo, "tracked.txt", "ordinary stage commit\n")
	recoveryTestGit(t, repo, "add", "tracked.txt")
	recoveryTestGit(t, repo, "commit", "-m", "ordinary stage after parent return")
	head := recoveryTestGit(t, repo, "rev-parse", "HEAD")
	childSnapshotWrite(t, repo, "tracked.txt", "latest staging\n")
	childSnapshotWrite(t, repo, "private/token", "runtime secret\n")
	childSnapshotWrite(t, repo, "private/committed", "injected runtime replacement\n")
	recoveryTestGit(t, repo, "add", "tracked.txt", "private")
	childSnapshotWrite(t, repo, "tracked.txt", "latest working files\n")
	childSnapshotWrite(t, repo, "untracked.bin", "\x00\xff")
	policy := SnapshotPolicy{ExcludedPaths: []string{"private"}}
	before := captureChildFixture(t, repo, key, "parent", at, policy)
	request := RetentionRequest{Repository: repo, RepositoryKey: key, RunID: "parent", BaseRef: base, IdentityTime: at, RetainUntil: at.Add(24 * time.Hour), InventoryRoot: t.TempDir(), CleanupRoots: []string{repo}, MaxSnapshots: 2, MaxArchiveBytes: 1 << 20, ParentPolicy: &policy}
	blocked := errors.New("journal acknowledgement unavailable")
	log := retentionJournalFunc(func(journal.Event) error { return blocked })
	if record, path, err := Retain(t.Context(), request, log); !errors.Is(err, blocked) || record != (Record{}) || path != "" {
		t.Fatalf("unacknowledged parent archive permits cleanup: %+v %s %v", record, path, err)
	}
	blocked = nil
	record, recordPath, err := Retain(t.Context(), request, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckChildSnapshotCurrent(t.Context(), repo, before); err != nil {
		t.Fatal("archive capture changed source", err)
	}
	again, againPath, err := Retain(t.Context(), request, log)
	if err != nil || again != record || againPath != recordPath {
		t.Fatal("archive replay changed identity", err)
	}
	// Import into a fresh repository holding only the original base. Later
	// ordinary commits, the index root, and working files must come from the
	// retained bundle, not from a surviving source worktree or worker output.
	restored := t.TempDir()
	recoveryTestGit(t, restored, "init")
	recoveryTestGit(t, restored, "fetch", repo, base)
	archive := filepath.Join(filepath.Dir(recordPath), BundleFileName)
	if err := ImportSnapshotBundle(t.Context(), restored, archive, record, request.MaxArchiveBytes); err != nil {
		t.Fatal(err)
	}
	state, err := ReadRetainedParentState(t.Context(), restored, record)
	if err != nil || state.HeadSHA != head {
		t.Fatal("archive lost original commit identity", state, err)
	}
	for ref, want := range map[string]string{state.HeadSHA + ":tracked.txt": "ordinary stage commit", state.IndexSHA + ":tracked.txt": "latest staging", record.SnapshotSHA + ":tracked.txt": "latest working files", record.SnapshotSHA + ":untracked.bin": "\x00\xff"} {
		if got := recoveryTestGit(t, restored, "show", ref); got != want {
			t.Fatalf("%s = %q, want %q", ref, got, want)
		}
	}
	for _, tree := range []string{state.IndexSHA, record.SnapshotSHA} {
		if got := recoveryTestGit(t, restored, "ls-tree", "-r", "--name-only", tree, "--", "private/token"); got != "" {
			t.Fatal("archive included excluded runtime state", got)
		}
	}
	if got := recoveryTestGit(t, restored, "show", record.SnapshotSHA+":private/committed"); got != "original repository value" {
		t.Fatal("exclusion became a repository deletion or injected replacement", got)
	}
	if got := recoveryTestGit(t, restored, "ls-tree", "-r", "--name-only", state.IndexSHA, "--", "private"); got != "" {
		t.Fatal("archive retained excluded staged injection", got)
	}
	if err := os.RemoveAll(repo); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRetainedParentState(t.Context(), restored, record); err != nil {
		t.Fatal("archive depended on removed checkout", err)
	}
}

func TestIntegrationParentRetentionKeepsStagingOnlyChanges(t *testing.T) {
	testdep.Require(t, "git")
	repo, key, at := childSnapshotFixture(t)
	childSnapshotWrite(t, repo, "tracked.txt", "staged only\n")
	recoveryTestGit(t, repo, "add", "tracked.txt")
	childSnapshotWrite(t, repo, "tracked.txt", "base\n")
	policy := SnapshotPolicy{}
	request := RetentionRequest{Repository: repo, RepositoryKey: key, RunID: "parent", BaseRef: "main", IdentityTime: at, RetainUntil: at.Add(time.Hour), InventoryRoot: t.TempDir(), CleanupRoots: []string{repo}, MaxSnapshots: 1, MaxArchiveBytes: 1 << 20, ParentPolicy: &policy, SkipEmpty: true}
	record, _, err := Retain(t.Context(), request, retentionJournalFunc(func(journal.Event) error { return nil }))
	if err != nil || record.SnapshotSHA == "" || record.PatchDigest != emptyPatchDigest {
		t.Fatal("staging-only state was discarded", record, err)
	}
	state, err := ReadRetainedParentState(t.Context(), repo, record)
	if err != nil {
		t.Fatal(err)
	}
	if got := recoveryTestGit(t, repo, "show", state.IndexSHA+":tracked.txt"); got != "staged only" {
		t.Fatal("index-only state missing", got)
	}
}
