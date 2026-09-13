//go:build integration

package recovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationRecoveryBundleSurvivesMissingSourceRepository(t *testing.T) {
	testdep.Require(t, "git")
	repository := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "base")
	record := storageTestRecord()
	record.BaseSHA = recoveryTestGit(t, repository, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repository, "payload.bin"), []byte{0, 255, 1, 0, 2}, 0o600); err != nil {
		t.Fatal(err)
	}
	recoveryTestGit(t, repository, "add", ".")
	recoveryTestGit(t, repository, "commit", "-m", "snapshot")
	record.SnapshotSHA = recoveryTestGit(t, repository, "rev-parse", "HEAD")
	record.PatchDigest = recoveryTestPatchDigest(t, repository, record)
	var archive bytes.Buffer
	digest, format, err := WriteSnapshotBundle(context.Background(), repository, record, &archive, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if format != archiveFormatFull {
		t.Fatalf("bundle with no BaseRef must fall back to full, got %q", format)
	}
	if expected := fmt.Sprintf("sha256:%x", sha256.Sum256(archive.Bytes())); digest != expected {
		t.Fatalf("bundle digest mismatch: %s != %s", digest, expected)
	}
	record.ArchiveDigest, record.ArchiveBytes, record.ArchiveFormat = digest, int64(archive.Len()), format
	var limited bytes.Buffer
	if digest, _, err := WriteSnapshotBundle(context.Background(), repository, record, &limited, 10); err == nil || digest != "" || limited.Len() > 10 {
		t.Fatalf("bundle byte budget was not enforced: digest=%q bytes=%d error=%v", digest, limited.Len(), err)
	}
	if digest, _, err := WriteSnapshotBundle(context.Background(), repository, record, failingPatchWriter{}, 1<<20); err == nil || digest != "" {
		t.Fatalf("failed bundle writer acknowledged capture: %q %v", digest, err)
	}
	archiveDirectory := t.TempDir()
	bundle := filepath.Join(archiveDirectory, BundleFileName)
	for range 2 {
		published, err := PublishRetainedState(context.Background(), repository, archiveDirectory, []string{repository}, record, 1<<20)
		if err != nil || published != record {
			t.Fatalf("durable retained publication/retry failed: %+v %v", published, err)
		}
		if stored, err := ReadRecord(filepath.Join(archiveDirectory, RecordFileName)); err != nil || stored != record {
			t.Fatalf("published metadata missing or mismatched: %+v %v", stored, err)
		}
	}
	// Move rather than delete the fixture: the verification repository has no
	// source location or alternates, so it must use the archive's own objects.
	if err := os.Rename(repository, repository+"-offline"); err != nil {
		t.Fatal(err)
	}
	restored := t.TempDir()
	recoveryTestGit(t, restored, "init", "--initial-branch=main")
	wrong := record
	wrong.ArchiveDigest = "sha256:" + strings.Repeat("0", 64)
	if err := ImportSnapshotBundle(context.Background(), restored, bundle, wrong, 1<<20); err == nil {
		t.Fatal("unverified archive imported")
	}
	wrong = record
	wrong.ArchiveBytes++
	if err := ImportSnapshotBundle(context.Background(), restored, bundle, wrong, 1<<20); err == nil {
		t.Fatal("archive with incorrect recorded size imported")
	}
	if refs := recoveryTestGit(t, restored, "for-each-ref", "--format=%(refname)"); refs != "" {
		t.Fatalf("bad digest created refs: %s", refs)
	}
	if err := ImportSnapshotBundle(context.Background(), restored, bundle, record, 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreSnapshot(context.Background(), restored, record, record.BaseSHA, "recovered", 1<<20); err != nil {
		t.Fatalf("restore after source removal: %v", err)
	}
	recoveryTestGit(t, restored, "cat-file", "-e", record.BaseSHA+"^{commit}")
	recoveryTestGit(t, restored, "cat-file", "-e", record.SnapshotSHA+":payload.bin")
}

// TestIntegrationRecoveryDeltaBundleScalesWithChangeNotBaseHistory pins #4862:
// a one-file change must produce an archive proportional to the diff, not to
// the size of the base repository's history, whenever the base is provably
// reachable from its own tracked ref.
func TestIntegrationRecoveryDeltaBundleScalesWithChangeNotBaseHistory(t *testing.T) {
	testdep.Require(t, "git")
	repository := t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "root")
	// Inflate the base history well past any reasonable tight per-diff bound:
	// a bug that bundles it all in would blow the budget below immediately.
	large := bytes.Repeat([]byte{0x42}, 4<<20)
	if err := os.WriteFile(filepath.Join(repository, "big.bin"), large, 0o600); err != nil {
		t.Fatal(err)
	}
	recoveryTestGit(t, repository, "add", ".")
	recoveryTestGit(t, repository, "commit", "-m", "large base payload")
	baseSHA := recoveryTestGit(t, repository, "rev-parse", "HEAD")
	// The tracked branch keeps moving past the recorded base — proving the
	// base is an ancestor of the branch tip, not the literal tip itself.
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "branch continues")

	record := storageTestRecord()
	record.BaseRef = "refs/heads/main"
	record.BaseSHA = baseSHA
	if err := os.WriteFile(filepath.Join(repository, "small.txt"), []byte("one file change"), 0o600); err != nil {
		t.Fatal(err)
	}
	recoveryTestGit(t, repository, "add", "small.txt")
	recoveryTestGit(t, repository, "commit", "-m", "one-file change")
	record.SnapshotSHA = recoveryTestGit(t, repository, "rev-parse", "HEAD")
	record.PatchDigest = recoveryTestPatchDigest(t, repository, record)

	var archive bytes.Buffer
	digest, format, err := WriteSnapshotBundle(context.Background(), repository, record, &archive, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if format != archiveFormatDelta {
		t.Fatalf("expected a delta bundle when the base is reachable from its tracked ref, got %q", format)
	}
	const tightBudget = 4096 // the 4 MiB base payload must not leak in
	if archive.Len() > tightBudget {
		t.Fatalf("one-file-change bundle was %d bytes, want <= %d: it scaled with base history", archive.Len(), tightBudget)
	}
	if expected := fmt.Sprintf("sha256:%x", sha256.Sum256(archive.Bytes())); digest != expected {
		t.Fatalf("bundle digest mismatch: %s != %s", digest, expected)
	}
	record.ArchiveDigest, record.ArchiveBytes, record.ArchiveFormat = digest, int64(archive.Len()), format
	archiveDirectory := t.TempDir()
	bundlePath := filepath.Join(archiveDirectory, BundleFileName)
	if err := os.WriteFile(bundlePath, archive.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	// A repository lacking the base must fail explicitly rather than import a
	// truncated, unrestorable history.
	missingBase := t.TempDir()
	recoveryTestGit(t, missingBase, "init", "--initial-branch=main")
	if err := ImportSnapshotBundle(context.Background(), missingBase, bundlePath, record, 1<<20); err == nil {
		t.Fatal("delta bundle imported into a repository missing its required base commit")
	}

	// With the base present — as it is in the managed mirror this delta is
	// designed for — import and restore reconstruct the exact snapshot.
	withBase := t.TempDir()
	recoveryTestGit(t, withBase, "init", "--initial-branch=main")
	recoveryTestGit(t, withBase, "fetch", repository, baseSHA+":refs/heads/mirrored-base")
	if err := ImportSnapshotBundle(context.Background(), withBase, bundlePath, record, 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreSnapshot(context.Background(), withBase, record, record.BaseSHA, "recovered", 1<<20); err != nil {
		t.Fatalf("restore from delta bundle: %v", err)
	}
	recoveryTestGit(t, withBase, "cat-file", "-e", record.SnapshotSHA+":small.txt")
}
