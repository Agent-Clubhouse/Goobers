//go:build integration && (linux || darwin)

package credentials

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/testdep"
)

func TestIntegrationGitEnvDrivesAskpassProtocol(t *testing.T) {
	testdep.Require(t, "sh")

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
