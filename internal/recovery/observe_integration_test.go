//go:build integration

package recovery

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

// fakeSnapshotObserver records every SnapshotObserver call for assertion. It
// must be safe for concurrent use per the interface contract, even though
// these tests call it from a single goroutine.
type fakeSnapshotObserver struct {
	mu              sync.Mutex
	capturedFormats []string
	capturedBytes   []int64
	fallbackReasons []string
	restoreFailures []string
}

func (f *fakeSnapshotObserver) SnapshotCaptured(format string, bytes int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.capturedFormats = append(f.capturedFormats, format)
	f.capturedBytes = append(f.capturedBytes, bytes)
}

func (f *fakeSnapshotObserver) SnapshotFallback(reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fallbackReasons = append(f.fallbackReasons, reason)
}

func (f *fakeSnapshotObserver) SnapshotRestoreFailed(reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restoreFailures = append(f.restoreFailures, reason)
}

// TestIntegrationWriteSnapshotBundleObservesFallbackAndCapture pins #5028: a
// capture with no BaseRef must report FallbackReasonNoBaseRef and never a
// captured event for the format it never selected... except WriteSnapshotBundle
// always reports the format it DID select, full or delta, on success.
func TestIntegrationWriteSnapshotBundleObservesFallbackAndCapture(t *testing.T) {
	testdep.Require(t, "git")
	repository := t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "base")
	record := storageTestRecord()
	record.BaseSHA = recoveryTestGit(t, repository, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repository, "payload.bin"), []byte{1, 2, 3}, 0o600); err != nil {
		t.Fatal(err)
	}
	recoveryTestGit(t, repository, "add", ".")
	recoveryTestGit(t, repository, "commit", "-m", "snapshot")
	record.SnapshotSHA = recoveryTestGit(t, repository, "rev-parse", "HEAD")
	record.PatchDigest = recoveryTestPatchDigest(t, repository, record)

	observer := &fakeSnapshotObserver{}
	ctx := WithSnapshotObserver(context.Background(), observer)

	var archive bytes.Buffer
	digest, format, err := WriteSnapshotBundle(ctx, repository, record, &archive, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if format != archiveFormatFull {
		t.Fatalf("expected fallback to full with no BaseRef, got %q", format)
	}
	if digest == "" {
		t.Fatal("expected a digest on successful capture")
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if got := observer.fallbackReasons; len(got) != 1 || got[0] != string(FallbackReasonNoBaseRef) {
		t.Fatalf("expected one no_base_ref fallback observation, got %v", got)
	}
	if got := observer.capturedFormats; len(got) != 1 || got[0] != archiveFormatFull {
		t.Fatalf("expected one full capture observation, got %v", got)
	}
	if got := observer.capturedBytes; len(got) != 1 || got[0] != int64(archive.Len()) {
		t.Fatalf("expected captured bytes %d, got %v", archive.Len(), got)
	}
}

// TestIntegrationWriteSnapshotBundleObservesDeltaCaptureWithoutFallback pins
// #5028: a capture that successfully selects the delta format must report no
// fallback and a single delta capture observation.
func TestIntegrationWriteSnapshotBundleObservesDeltaCaptureWithoutFallback(t *testing.T) {
	testdep.Require(t, "git")
	repository := t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "root")
	baseSHA := recoveryTestGit(t, repository, "rev-parse", "HEAD")
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "branch continues")

	record := storageTestRecord()
	record.BaseRef = "refs/heads/main"
	record.BaseSHA = baseSHA
	if err := os.WriteFile(filepath.Join(repository, "small.txt"), []byte("change"), 0o600); err != nil {
		t.Fatal(err)
	}
	recoveryTestGit(t, repository, "add", "small.txt")
	recoveryTestGit(t, repository, "commit", "-m", "one-file change")
	record.SnapshotSHA = recoveryTestGit(t, repository, "rev-parse", "HEAD")
	record.PatchDigest = recoveryTestPatchDigest(t, repository, record)

	observer := &fakeSnapshotObserver{}
	ctx := WithSnapshotObserver(context.Background(), observer)

	var archive bytes.Buffer
	_, format, err := WriteSnapshotBundle(ctx, repository, record, &archive, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if format != archiveFormatDelta {
		t.Fatalf("expected a delta capture, got %q", format)
	}

	observer.mu.Lock()
	defer observer.mu.Unlock()
	if len(observer.fallbackReasons) != 0 {
		t.Fatalf("delta capture must not report a fallback, got %v", observer.fallbackReasons)
	}
	if got := observer.capturedFormats; len(got) != 1 || got[0] != archiveFormatDelta {
		t.Fatalf("expected one delta capture observation, got %v", got)
	}
}

// TestIntegrationImportSnapshotBundleObservesRestoreFailures pins #5028:
// ImportSnapshotBundle must label a missing-base restore failure distinctly
// from an archive verification failure.
func TestIntegrationImportSnapshotBundleObservesRestoreFailures(t *testing.T) {
	testdep.Require(t, "git")
	repository := t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "root")
	baseSHA := recoveryTestGit(t, repository, "rev-parse", "HEAD")
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "branch continues")

	record := storageTestRecord()
	record.BaseRef = "refs/heads/main"
	record.BaseSHA = baseSHA
	if err := os.WriteFile(filepath.Join(repository, "small.txt"), []byte("change"), 0o600); err != nil {
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
	record.ArchiveDigest, record.ArchiveBytes, record.ArchiveFormat = digest, int64(archive.Len()), format
	archiveDirectory := t.TempDir()
	bundlePath := filepath.Join(archiveDirectory, BundleFileName)
	if err := os.WriteFile(bundlePath, archive.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	// Missing base: the receiving repository never fetched baseSHA.
	observer := &fakeSnapshotObserver{}
	ctx := WithSnapshotObserver(context.Background(), observer)
	missingBase := t.TempDir()
	recoveryTestGit(t, missingBase, "init", "--initial-branch=main")
	if err := ImportSnapshotBundle(ctx, missingBase, bundlePath, record, 1<<20); err == nil {
		t.Fatal("expected import to fail without the required base commit")
	}
	observer.mu.Lock()
	if got := observer.restoreFailures; len(got) != 1 || got[0] != string(RestoreFailureReasonBaseMissing) {
		t.Fatalf("expected one base_missing restore failure, got %v", got)
	}
	observer.mu.Unlock()

	// Archive verification failure: a record whose digest cannot match.
	observer2 := &fakeSnapshotObserver{}
	ctx2 := WithSnapshotObserver(context.Background(), observer2)
	wrong := record
	wrong.ArchiveBytes++
	withBase := t.TempDir()
	recoveryTestGit(t, withBase, "init", "--initial-branch=main")
	recoveryTestGit(t, withBase, "fetch", repository, baseSHA+":refs/heads/mirrored-base")
	if err := ImportSnapshotBundle(ctx2, withBase, bundlePath, wrong, 1<<20); err == nil {
		t.Fatal("expected import to fail for a mismatched archive size")
	}
	observer2.mu.Lock()
	if got := observer2.restoreFailures; len(got) != 1 || got[0] != string(RestoreFailureReasonArchiveInvalid) {
		t.Fatalf("expected one archive_invalid restore failure, got %v", got)
	}
	observer2.mu.Unlock()

	// Success: no restore failure reported.
	observer3 := &fakeSnapshotObserver{}
	ctx3 := WithSnapshotObserver(context.Background(), observer3)
	if err := ImportSnapshotBundle(ctx3, withBase, bundlePath, record, 1<<20); err != nil {
		t.Fatalf("expected import to succeed with the base present: %v", err)
	}
	observer3.mu.Lock()
	defer observer3.mu.Unlock()
	if len(observer3.restoreFailures) != 0 {
		t.Fatalf("successful import must not report a restore failure, got %v", observer3.restoreFailures)
	}
}
