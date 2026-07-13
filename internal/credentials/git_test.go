package credentials

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat askpass script: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("askpass script perms = %v, want no group/other access", info.Mode().Perm())
	}
}

func TestGitEnvDrivesAskpassProtocol(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	dir := t.TempDir()
	path, err := WriteAskpassScript(dir)
	if err != nil {
		t.Fatalf("WriteAskpassScript: %v", err)
	}
	env := GitEnv(path, "canary-token-value")

	cmd := exec.Command(path, "Password for 'https://example.com':")
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("run askpass script: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "canary-token-value" {
		t.Fatalf("askpass password output = %q, want %q", got, "canary-token-value")
	}

	cmd = exec.Command(path, "Username for 'https://example.com':")
	cmd.Env = append(os.Environ(), env...)
	out.Reset()
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("run askpass script: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got == "" || got == "canary-token-value" {
		t.Fatalf("askpass username output = %q, want a non-empty non-token placeholder", got)
	}
}
