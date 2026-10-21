package temporaltest

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestResolveDevServerAcquisition covers the decision StartDevServer makes
// from CLIEnvVar's raw value, independent of actually spawning a dev server
// process or touching the network (#3393).
func TestResolveDevServerAcquisition(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "temporal")
	if err := os.WriteFile(executable, []byte("test"), 0o755); err != nil {
		t.Fatal(err)
	}
	acquisition, err := resolveDevServerAcquisition(executable)
	if err != nil {
		t.Fatal(err)
	}
	if acquisition.path != executable || acquisition.download || !strings.Contains(acquisition.mode, "existing CLI") {
		t.Errorf("existing executable resolved as %+v", acquisition)
	}

	missing := filepath.Join(t.TempDir(), "temporal")
	acquisition, err = resolveDevServerAcquisition(missing)
	if err != nil {
		t.Fatal(err)
	}
	if acquisition.path != missing || !acquisition.download {
		t.Errorf("missing configured path resolved as %+v, want download into %q", acquisition, missing)
	}
	if !strings.Contains(acquisition.mode, "missing; downloading") {
		t.Errorf("missing configured path mode = %q, want explicit fallback log", acquisition.mode)
	}

	nonExecutable := filepath.Join(t.TempDir(), "temporal")
	if err := os.WriteFile(nonExecutable, []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveDevServerAcquisition(nonExecutable); err == nil || !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("non-executable configured path error = %v, want clear executable error", err)
	}
}

func TestDevServerCLIVersionIsPinned(t *testing.T) {
	if !regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(CLIVersion) {
		t.Fatalf("CLIVersion = %q, want an explicit release, not a floating download alias", CLIVersion)
	}
}
