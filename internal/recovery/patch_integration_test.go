//go:build integration

package recovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationRecoveryPatchDigestCoversBinaryAndRefusesMismatch(t *testing.T) {
	testdep.Require(t, "git")
	repository := t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "base")
	record := storageTestRecord()
	record.BaseSHA = recoveryTestGit(t, repository, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repository, "binary.bin"), []byte{0, 255, 1, 0, 2}, 0o600); err != nil {
		t.Fatal(err)
	}
	recoveryTestGit(t, repository, "add", ".")
	recoveryTestGit(t, repository, "commit", "-m", "binary snapshot")
	record.SnapshotSHA = recoveryTestGit(t, repository, "rev-parse", "HEAD")
	var patch bytes.Buffer
	digest, err := WriteSnapshotPatch(context.Background(), repository, record.BaseSHA, record.SnapshotSHA, &patch)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("sha256:%x", sha256.Sum256(patch.Bytes())); digest != want {
		t.Fatalf("digest does not cover emitted bytes: %s != %s", digest, want)
	}
	if !strings.Contains(patch.String(), "GIT binary patch") {
		t.Fatalf("binary content was reduced to a display-only diff: %s", patch.String())
	}
	record.PatchDigest = "sha256:" + strings.Repeat("0", 64)
	if err := PinCommit(context.Background(), repository, record); err == nil {
		t.Fatal("mismatched patch digest accepted")
	}
	if refs := recoveryTestGit(t, repository, "for-each-ref", "--format=%(refname)", record.Ref); refs != "" {
		t.Fatalf("mismatch published a recovery ref: %s", refs)
	}
	if failedDigest, err := WriteSnapshotPatch(context.Background(), repository, record.BaseSHA, record.SnapshotSHA, failingPatchWriter{}); err == nil || failedDigest != "" {
		t.Fatalf("failed output acknowledged a digest: %q %v", failedDigest, err)
	}
	for _, setting := range [][2]string{{"diff.noprefix", "true"}, {"diff.context", "20"}, {"diff.algorithm", "histogram"}, {"color.ui", "always"}} {
		recoveryTestGit(t, repository, "config", setting[0], setting[1])
	}
	if got := recoveryTestPatchDigest(t, repository, record); got != digest {
		t.Fatalf("display preferences changed recovery bytes: %s != %s", got, digest)
	}
	record.PatchDigest = digest
	if err := PinCommit(context.Background(), repository, record); err != nil {
		t.Fatalf("verified patch could not be pinned: %v", err)
	}
}

type failingPatchWriter struct{}

func (failingPatchWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
