//go:build integration

package recovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
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
	digest, err := WriteSnapshotBundle(context.Background(), repository, record, &archive, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if expected := fmt.Sprintf("sha256:%x", sha256.Sum256(archive.Bytes())); digest != expected {
		t.Fatalf("bundle digest mismatch: %s != %s", digest, expected)
	}
	var limited bytes.Buffer
	if digest, err := WriteSnapshotBundle(context.Background(), repository, record, &limited, 10); err == nil || digest != "" || limited.Len() > 10 {
		t.Fatalf("bundle byte budget was not enforced: digest=%q bytes=%d error=%v", digest, limited.Len(), err)
	}
	if digest, err := WriteSnapshotBundle(context.Background(), repository, record, failingPatchWriter{}, 1<<20); err == nil || digest != "" {
		t.Fatalf("failed bundle writer acknowledged capture: %q %v", digest, err)
	}
	bundle := filepath.Join(t.TempDir(), "recovery.bundle")
	for range 2 {
		if published, err := PublishSnapshotBundle(context.Background(), repository, bundle, record, 1<<20); err != nil || published != digest {
			t.Fatalf("durable archive publication/retry failed: %q %v", published, err)
		}
	}
	// Move rather than delete the fixture: the verification repository has no
	// source location or alternates, so it must use the archive's own objects.
	if err := os.Rename(repository, repository+"-offline"); err != nil {
		t.Fatal(err)
	}
	restored := t.TempDir()
	recoveryTestGit(t, restored, "init", "--initial-branch=main")
	recoveryTestGit(t, restored, "fetch", bundle, record.SnapshotSHA+":refs/heads/recovered")
	recoveryTestGit(t, restored, "cat-file", "-e", record.BaseSHA+"^{commit}")
	recoveryTestGit(t, restored, "cat-file", "-e", record.SnapshotSHA+":payload.bin")
}
