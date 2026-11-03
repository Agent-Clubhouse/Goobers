package journal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestArtifactBytesRootedSymlinkContainment(t *testing.T) {
	dir := t.TempDir()
	data := []byte("content")
	inside := filepath.Join(dir, "inside")
	outside := filepath.Join(t.TempDir(), "outside")
	for _, path := range []string{inside, outside} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, target := range map[string]string{"safe": "inside", "escape": outside} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Skipf("symlinks unsupported: %v", err)
		}
	}
	reader := &Reader{dir: dir}
	ref := Ref{Path: "safe", Digest: Digest(data)}
	if got, err := reader.ArtifactBytes(ref); err != nil || string(got) != string(data) {
		t.Fatalf("internal symlink = %q, %v", got, err)
	}
	ref.Path = "escape"
	if _, err := reader.ArtifactBytes(ref); err == nil {
		t.Fatal("read an artifact through a symlink outside the run directory")
	}
}
