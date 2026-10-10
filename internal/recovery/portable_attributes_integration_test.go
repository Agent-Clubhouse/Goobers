//go:build integration

package recovery

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationPortableWorkspacePreservesCapturedBytes(t *testing.T) {
	testdep.Require(t, "git")
	repo, key, at := childSnapshotFixture(t)
	files := map[string]string{
		".gitattributes":        "*.txt text eol=crlf\n*.id ident\n*.utf working-tree-encoding=UTF-16LE\n",
		"tracked.txt":           "captured LF bytes\n",
		"nested/.gitattributes": "*.txt text eol=lf\n",
		"nested/raw.txt":        "captured CRLF bytes\r\n",
		"expanded.id":           "$Id$\n",
		"encoded.utf":           "captured UTF-8 bytes\n",
		"raw.bin":               "\x00\xff\x01",
	}
	for name, content := range files {
		childSnapshotWrite(t, repo, name, content)
	}
	headBefore := recoveryTestGit(t, repo, "rev-parse", "HEAD")
	indexBefore, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := captureChildFixture(t, repo, key, "child", at, SnapshotPolicy{})
	var bundle bytes.Buffer
	portable, err := WritePortableSnapshot(t.Context(), repo, snapshot, &bundle, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "input.bundle")
	if err = os.WriteFile(archive, bundle.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	pod := t.TempDir()
	if err = InitializePortableWorkspace(t.Context(), pod, archive, portable, 16<<20); err != nil {
		t.Fatal(err)
	}
	for name, want := range files {
		for _, root := range []string{pod, repo} {
			got, err := os.ReadFile(filepath.Join(root, name))
			if err != nil || string(got) != want {
				t.Errorf("%s in %s = %q, want %q: %v", name, root, got, want, err)
			}
		}
	}
	if got := recoveryTestGit(t, pod, "status", "--porcelain"); got != "" {
		t.Errorf("materialization changed the captured tree: %s", got)
	}
	indexAfter, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil || !bytes.Equal(indexBefore, indexAfter) || recoveryTestGit(t, repo, "rev-parse", "HEAD") != headBefore {
		t.Fatal("portable capture modified source Git state", err)
	}
}
