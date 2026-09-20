//go:build integration

package recovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// TestIntegrationOverflowRestoresTheSameTreeAsABundle is the durability claim
// of the ref tier stated as a comparison, not as an assertion about paths: an
// overflow entry must restore byte-for-byte what a bundle of the same snapshot
// restores. If that were ever untrue, overflowing instead of refusing would be
// trading a stalled instance for silently degraded recovery.
func TestIntegrationOverflowRestoresTheSameTreeAsABundle(t *testing.T) {
	testdep.Require(t, "git")
	source, inventory, overflowRoot := t.TempDir(), t.TempDir(), t.TempDir()
	recoveryTestGit(t, source, "init", "--initial-branch=main")
	recoveryTestGit(t, source, "commit", "--allow-empty", "-m", "base")
	if err := os.WriteFile(filepath.Join(source, "work.txt"), []byte("agent-authored work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	template := storageTestRecord()
	request := RetentionRequest{
		Repository: source, RepositoryKey: template.RepositoryKey, RunID: template.RunID,
		BaseRef: "main", IdentityTime: template.CreatedAt, RetainUntil: template.RetainUntil,
		InventoryRoot: inventory, CleanupRoots: []string{source}, MaxSnapshots: 1, MaxArchiveBytes: 1 << 20,
	}
	bundled, recordPath, err := Retain(context.Background(), request, retentionJournalFunc(func(journal.Event) error { return nil }))
	if err != nil {
		t.Fatalf("retain: %v", err)
	}
	// The same immutable snapshot, published to the overflow tier: same ref,
	// same patch digest, no archive.
	overflowRecord := bundled
	overflowRecord.ArchiveDigest, overflowRecord.ArchiveBytes, overflowRecord.ArchiveFormat = "", 0, ""
	if _, _, err := PublishOverflow(context.Background(), source, overflowRoot, overflowRecord); err != nil {
		t.Fatalf("publish overflow: %v", err)
	}

	fromBundle := restoreIntoFreshClone(t, source, bundled, func(destination string) error {
		return ImportSnapshotBundle(context.Background(), destination, filepath.Join(filepath.Dir(recordPath), BundleFileName), bundled, 1<<20)
	})
	fromOverflow := restoreIntoFreshClone(t, source, overflowRecord, func(destination string) error {
		return ImportSnapshotFromRepository(context.Background(), destination, source, overflowRecord)
	})
	if fromBundle != fromOverflow {
		t.Fatalf("restoring from the overflow tier produced a different tree than the bundle: %s != %s", fromBundle, fromOverflow)
	}
}

// restoreIntoFreshClone restores record into a clone of source and returns the
// restored commit's tree. The tree, not the commit, is the comparison: the two
// restorations happen at different wall-clock times, so their commit IDs
// legitimately differ while their content must not.
func restoreIntoFreshClone(t *testing.T, source string, record Record, importObjects func(string) error) string {
	t.Helper()
	destination := t.TempDir()
	recoveryTestGit(t, destination, "clone", "--no-local", "--", source, ".")
	base := recoveryTestGit(t, destination, "rev-parse", record.BaseSHA)
	if err := importObjects(destination); err != nil {
		t.Fatalf("import: %v", err)
	}
	commit, err := RestoreSnapshot(context.Background(), destination, record, base, "restored", 1<<20)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	return recoveryTestGit(t, destination, "rev-parse", commit+"^{tree}")
}

// TestIntegrationOverflowReadsOldestFirstAndTolerates covers what promotion
// depends on: the tier is ordered by capture time, not by the identity hash
// its directories are named after, and one piece of debris does not hide the
// readable entries beside it.
func TestIntegrationOverflowReadsOldestFirstAndTolerates(t *testing.T) {
	testdep.Require(t, "git")
	source, root := t.TempDir(), t.TempDir()
	recoveryTestGit(t, source, "init", "--initial-branch=main")
	recoveryTestGit(t, source, "commit", "--allow-empty", "-m", "base")
	base := recoveryTestGit(t, source, "rev-parse", "HEAD")
	template := storageTestRecord()
	var published []Record
	for index, name := range []string{"newest", "middle", "oldest"} {
		record := overflowSnapshot(t, source, base, template.RepositoryKey, name, time.Duration(index)*time.Hour)
		if _, _, err := PublishOverflow(context.Background(), source, root, record); err != nil {
			t.Fatalf("publish %s: %v", name, err)
		}
		published = append(published, record)
	}
	if err := os.MkdirAll(filepath.Join(root, "debris"), 0o700); err != nil {
		t.Fatal(err)
	}
	entries, unreadable, err := ReadOverflow(context.Background(), root)
	if err != nil {
		t.Fatalf("read overflow: %v", err)
	}
	if len(entries) != 3 || len(unreadable) != 1 {
		t.Fatalf("debris was not tolerated and reported: entries=%d unreadable=%d", len(entries), len(unreadable))
	}
	// Published newest-first; read back must be oldest-first.
	for index, entry := range entries {
		want := published[len(published)-1-index]
		if entry.Record.RunID != want.RunID {
			t.Fatalf("overflow entry %d is %s, want %s (oldest capture first)", index, entry.Record.RunID, want.RunID)
		}
	}
}

// TestIntegrationOverflowRecordRefusesAnArchiveClaim pins the one schema rule
// the tier adds: an overflow record must not claim bytes it does not have,
// because an importer would act on that claim.
func TestIntegrationOverflowRecordRefusesAnArchiveClaim(t *testing.T) {
	testdep.Require(t, "git")
	source, root := t.TempDir(), t.TempDir()
	recoveryTestGit(t, source, "init", "--initial-branch=main")
	recoveryTestGit(t, source, "commit", "--allow-empty", "-m", "base")
	base := recoveryTestGit(t, source, "rev-parse", "HEAD")
	record := overflowSnapshot(t, source, base, storageTestRecord().RepositoryKey, "claims-an-archive", 0)
	record.ArchiveDigest, record.ArchiveBytes = "sha256:"+record.SnapshotSHA+record.SnapshotSHA[:24], 64
	if _, _, err := PublishOverflow(context.Background(), source, root, record); err == nil {
		t.Fatal("an overflow record claiming an archive was published")
	}
	// A conflicting identity for the same directory is refused, never replaced.
	clean := record
	clean.ArchiveDigest, clean.ArchiveBytes = "", 0
	if _, _, err := PublishOverflow(context.Background(), source, root, clean); err != nil {
		t.Fatalf("publish: %v", err)
	}
	conflicting := clean
	conflicting.BaseRef = "refs/heads/other"
	if _, _, err := PublishOverflow(context.Background(), source, root, conflicting); !errors.Is(err, ErrRecordConflict) {
		t.Fatalf("a conflicting overflow identity was accepted: %v", err)
	}
}

func overflowSnapshot(t *testing.T, repository, base, repositoryKey, runID string, age time.Duration) Record {
	t.Helper()
	recoveryTestGit(t, repository, "checkout", "-b", "snap-"+runID, base)
	if err := os.WriteFile(filepath.Join(repository, runID+".txt"), []byte("work for "+runID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recoveryTestGit(t, repository, "add", "--all")
	recoveryTestGit(t, repository, "commit", "-m", "snapshot "+runID)
	snapshot := recoveryTestGit(t, repository, "rev-parse", "HEAD")
	recoveryTestGit(t, repository, "checkout", "main")
	ref, err := RefForSnapshot(runID, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := WriteSnapshotPatch(context.Background(), repository, base, snapshot, discardWriter{})
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Now().UTC().Add(-age)
	return Record{
		Version: 1, RunID: runID, RepositoryKey: repositoryKey, Ref: ref,
		BaseSHA: base, SnapshotSHA: snapshot, PatchDigest: digest,
		CreatedAt: createdAt, RetainUntil: createdAt.Add(29 * 24 * time.Hour),
	}
}

type discardWriter struct{}

func (discardWriter) Write(data []byte) (int, error) { return len(data), nil }
