package credentials

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestAskpassScriptsHonorUsernamePromptContract guards that the POSIX and
// Windows askpass helpers stay behaviorally identical (#1383): a "Username"
// prompt is answered from GOOBERS_GIT_USERNAME (defaulting to x-access-token),
// every other prompt from the token. A Windows helper that ignores the prompt
// and returns the token for the username too diverges from this contract and
// leaks the token into git's username field, so assert both scripts carry the
// full branch. (Runtime-executing a .cmd is not portable in CI, so this pins
// the contract structurally.)
func TestAskpassScriptsHonorUsernamePromptContract(t *testing.T) {
	for _, tc := range []struct{ name, script string }{
		{"posix", askpassScript},
		{"windows", askpassScriptWindows},
	} {
		for _, want := range []string{"Username", "GOOBERS_GIT_USERNAME", "x-access-token", "GOOBERS_GIT_TOKEN"} {
			if !strings.Contains(tc.script, want) {
				t.Errorf("%s askpass script does not reference %q; Username-prompt handling diverges from the shared contract", tc.name, want)
			}
		}
	}
}

func TestWriteAskpassScriptContainsNoSecretMaterial(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteAskpassScript(dir)
	if err != nil {
		t.Fatalf("WriteAskpassScript: %v", err)
	}
	wantBase := askpassScriptName
	if runtime.GOOS == "windows" {
		wantBase = askpassScriptNameWindows
	}
	if got := filepath.Base(path); got != wantBase {
		t.Fatalf("askpass helper name = %q, want %q", got, wantBase)
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
