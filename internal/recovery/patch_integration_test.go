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

// Regression for #6306: a worktree's diff drivers (diff=csharp changes hunk
// headers) must not make the worker's digest differ from a bare mirror's.
func TestIntegrationRecoveryPatchDigestIgnoresWorktreeAttributes(t *testing.T) {
	testdep.Require(t, "git")
	ctx := context.Background()
	repository := t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	body := "namespace N\n{\n    public class C\n    {\n        public int Method()\n        {\n            var a = 1;\n            var b = 2;\n            var c = 3;\n            var d = 4;\n            return a;\n        }\n    }\n}\n"
	if err := os.WriteFile(filepath.Join(repository, ".gitattributes"), []byte("*.cs diff=csharp\n*.txt binary\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "C.cs"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "notes.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recoveryTestGit(t, repository, "add", ".")
	recoveryTestGit(t, repository, "commit", "-m", "base")
	base := recoveryTestGit(t, repository, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repository, "C.cs"), []byte(strings.Replace(body, "return a;", "return d;", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "notes.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recoveryTestGit(t, repository, "commit", "-am", "change")
	snapshot := recoveryTestGit(t, repository, "rev-parse", "HEAD")
	worktreeDigest, err := WriteSnapshotPatch(ctx, repository, base, snapshot, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	mirror := t.TempDir()
	recoveryTestGit(t, mirror, "init", "--bare")
	recoveryTestGit(t, mirror, "fetch", repository, "main:refs/heads/main")
	var patch bytes.Buffer
	mirrorDigest, err := WriteSnapshotPatch(ctx, mirror, base, snapshot, &patch)
	if err != nil {
		t.Fatal(err)
	}
	if worktreeDigest != mirrorDigest {
		t.Fatalf("worktree digest %s != mirror digest %s", worktreeDigest, mirrorDigest)
	}
	if strings.Contains(patch.String(), "public int Method()") && strings.Contains(patch.String(), "@@ -8,7 +8,7 @@ ") && strings.Contains(patch.String(), "GIT binary patch") {
		t.Fatalf("patch applied worktree attributes:\n%s", patch.String())
	}
}

// Regression for #6297: Git resolves a relative -O path against the
// repository prefix, so os.DevNull ("NUL" on Windows) became "<prefix>/NUL"
// and capture exited 128 below the repository root.
func TestIntegrationRecoveryPatchDiffDisablesOrderFileBelowRepositoryRoot(t *testing.T) {
	testdep.Require(t, "git")
	ctx := context.Background()
	repository := t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "base")
	base := recoveryTestGit(t, repository, "rev-parse", "HEAD")
	if err := os.MkdirAll(filepath.Join(repository, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.txt", filepath.Join("sub", "b.txt")} {
		if err := os.WriteFile(filepath.Join(repository, name), []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	recoveryTestGit(t, repository, "add", ".")
	recoveryTestGit(t, repository, "commit", "-m", "snapshot")
	snapshot := recoveryTestGit(t, repository, "rev-parse", "HEAD")
	var captured bytes.Buffer
	if _, err := WriteSnapshotPatch(ctx, repository, base, snapshot, &captured); err != nil {
		t.Fatal(err)
	}
	order := filepath.Join(t.TempDir(), "order")
	if err := os.WriteFile(order, []byte("sub/*\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recoveryTestGit(t, repository, "config", "diff.orderFile", order)
	var nested bytes.Buffer
	if err := recoveryGit(ctx, filepath.Join(repository, "sub"), &nested, snapshotPatchDiffArgs(base, snapshot)...); err != nil {
		t.Fatalf("capture diff below repository root: %v", err)
	}
	if nested.String() != captured.String() || strings.Index(captured.String(), "a/a.txt") > strings.Index(captured.String(), "a/sub/b.txt") {
		t.Fatalf("configured order or working directory changed recovery bytes:\n%s\nwant:\n%s", nested.String(), captured.String())
	}
}
