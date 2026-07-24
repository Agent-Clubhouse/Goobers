package credentials

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitAuthEnvironmentReplacesAmbientCredentialConfig(t *testing.T) {
	// Ambient credential configuration a host might carry — all of it must be
	// stripped so the configured token is the only credential git can reach.
	t.Setenv("GIT_ASKPASS", "/ambient/askpass")
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	t.Setenv("GOOBERS_GIT_TOKEN", "stale-token")
	t.Setenv("GOOBERS_GIT_USERNAME", "ambient-user")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "http.extraheader")
	t.Setenv("GIT_CONFIG_VALUE_0", "AUTHORIZATION: basic stale")
	// An unrelated variable must pass through: the returned slice is the
	// complete child environment, so dropping the base env would break git
	// itself (PATH, HOME, ...).
	t.Setenv("GOOBERS_TEST_PASSTHROUGH", "kept")

	env := GitAuthEnvironment("/scripts/goobers-askpass.sh", "fresh-token")

	values := map[string][]string{}
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		values[name] = append(values[name], value)
	}
	for name, want := range map[string]string{
		"GIT_ASKPASS":              "/scripts/goobers-askpass.sh",
		"GOOBERS_GIT_TOKEN":        "fresh-token",
		"GIT_TERMINAL_PROMPT":      "0",
		"GIT_CONFIG_COUNT":         "1",
		"GIT_CONFIG_KEY_0":         "credential.helper",
		"GIT_CONFIG_VALUE_0":       "",
		"GOOBERS_TEST_PASSTHROUGH": "kept",
	} {
		if got := values[name]; len(got) != 1 || got[0] != want {
			t.Errorf("%s = %q, want exactly [%q]", name, got, want)
		}
	}
	if got, present := values["GOOBERS_GIT_USERNAME"]; present {
		t.Errorf("GOOBERS_GIT_USERNAME = %q, want removed (deterministic askpass username)", got)
	}
	if _, present := values["PATH"]; !present {
		t.Errorf("PATH missing: base process environment was not preserved")
	}
}

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
