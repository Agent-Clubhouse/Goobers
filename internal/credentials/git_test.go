package credentials

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteAskpassScriptContainsNoSecretMaterial(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteAskpassScript(dir)
	if err != nil {
		t.Fatalf("WriteAskpassScript: %v", err)
	}
	fakeToken := "ghp_shouldNeverAppearOnDiskAnywhere"
	// Exercise the full seam as a caller would: resolve, then build the env
	// a git child process would receive.
	_ = GitEnv(path, fakeToken)

	// Scan test (issue #14 acceptance): no credential material in any file
	// under this directory.
	err = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rErr := os.ReadFile(p)
		if rErr != nil {
			return rErr
		}
		if bytes.Contains(b, []byte(fakeToken)) {
			t.Errorf("file %s contains credential material", p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}

	assertAskpassProtected(t, path)
}
