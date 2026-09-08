//go:build integration

package recovery

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationRetainedPublicationRetryIgnoresPackingChanges(t *testing.T) {
	testdep.Require(t, "git")
	ctx := context.Background()
	repository, directory := t.TempDir(), t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "base")
	if err := os.WriteFile(filepath.Join(repository, "payload.txt"), []byte(strings.Repeat("retained implementation bytes\n", 4096)), 0o600); err != nil {
		t.Fatal(err)
	}
	template := storageTestRecord()
	prepared, err := PrepareRecord(ctx, repository, template.RepositoryKey, template.RunID, "main", template.CreatedAt, template.RetainUntil)
	if err != nil {
		t.Fatal(err)
	}
	recoveryTestGit(t, repository, "config", "pack.compression", "0")
	first, err := PublishRetainedState(ctx, repository, directory, []string{repository}, prepared, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	recoveryTestGit(t, repository, "config", "pack.compression", "9")
	got, err := PublishRetainedState(ctx, repository, directory, []string{repository}, prepared, 1<<20)
	if err != nil || got != first {
		t.Fatalf("packing change invalidated a published snapshot retry: %+v %v", got, err)
	}
	archive := filepath.Join(directory, BundleFileName)
	before, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	conflict := prepared
	conflict.PatchDigest = "sha256:" + strings.Repeat("0", 64)
	if got, err := PublishRetainedState(ctx, repository, directory, []string{repository}, conflict, 1<<20); !errors.Is(err, ErrRecordConflict) || got != (Record{}) {
		t.Fatalf("conflicting retry acknowledged: %+v %v", got, err)
	}
	if got, err := PublishRetainedState(ctx, repository, directory, []string{repository}, prepared, first.ArchiveBytes-1); err == nil || got != (Record{}) {
		t.Fatalf("oversized retry acknowledged: %+v %v", got, err)
	}
	if after, err := os.ReadFile(archive); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("rejected retry changed evidence: %v", err)
	}
	corrupt := append(bytes.Clone(before), '\n')
	if err := os.WriteFile(archive, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := PublishRetainedState(ctx, repository, directory, []string{repository}, prepared, 1<<20); err == nil || got != (Record{}) {
		t.Fatalf("corrupted archive retry acknowledged: %+v %v", got, err)
	}
	if after, err := os.ReadFile(archive); err != nil || !bytes.Equal(corrupt, after) {
		t.Fatalf("retry silently replaced corrupted evidence: %v", err)
	}
}

func TestIntegrationRetainedPublicationMetadataFailurePreservesArchive(t *testing.T) {
	testdep.Require(t, "git")
	repository, directory := t.TempDir(), t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "base")
	record := storageTestRecord()
	record.BaseSHA = recoveryTestGit(t, repository, "rev-parse", "HEAD")
	record.SnapshotSHA = record.BaseSHA
	record.PatchDigest = recoveryTestPatchDigest(t, repository, record)
	record.ArchiveDigest, record.ArchiveBytes = "", 0
	// Seed the archive-first crash window explicitly. Malformed metadata must
	// fail before publication, while preserving evidence already on disk.
	if _, err := PublishSnapshotBundle(context.Background(), repository, filepath.Join(directory, BundleFileName), record, 1<<20); err != nil {
		t.Fatal(err)
	}
	metadata := filepath.Join(directory, RecordFileName)
	if err := os.Mkdir(metadata, 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := PublishRetainedState(context.Background(), repository, directory, []string{repository}, record, 1<<20)
	if err == nil || got != (Record{}) {
		t.Fatalf("metadata failure acknowledged: %+v %v", got, err)
	}
	archive := filepath.Join(directory, BundleFileName)
	before, err := os.ReadFile(archive)
	if err != nil || len(before) == 0 {
		t.Fatalf("recoverable archive lost after metadata failure: %v", err)
	}
	// Remove only the empty test-created blocker, then retry the same identity.
	if err := os.Remove(metadata); err != nil {
		t.Fatal(err)
	}
	foreign := record
	foreign.PatchDigest = "sha256:" + strings.Repeat("0", 64)
	if got, err := PublishRetainedState(context.Background(), repository, directory, []string{repository}, foreign, 1<<20); err == nil || got != (Record{}) {
		t.Fatalf("archive-only retry accepted a different prepared identity: %+v %v", got, err)
	}
	if _, err := os.Lstat(metadata); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unverified archive received metadata: %v", err)
	}
	recoveryTestGit(t, repository, "config", "pack.compression", "0")
	got, err = PublishRetainedState(context.Background(), repository, directory, []string{repository}, record, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(archive)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("retry changed archive evidence: %v", err)
	}
	if stored, err := ReadRecord(metadata); err != nil || stored != got {
		t.Fatalf("retry did not publish bound metadata: %+v %v", stored, err)
	}
	limited := t.TempDir()
	got, err = PublishRetainedState(context.Background(), repository, limited, []string{repository}, record, 10)
	if err == nil || got != (Record{}) {
		t.Fatalf("archive budget failure acknowledged: %+v %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(limited, RecordFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata published without complete archive: %v", err)
	}
}
