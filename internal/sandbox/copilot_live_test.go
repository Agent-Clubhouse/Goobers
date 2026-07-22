//go:build integration && darwin

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/testdep"
)

func TestIntegrationNativeSandboxCopilotLive(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_SANDBOX_COPILOT_LIVE")
	testdep.Require(t, "copilot")

	s := requiredNativeSandbox(t)
	workspace := t.TempDir()
	copilotHome := filepath.Join(workspace, ".copilot")
	temp := filepath.Join(workspace, ".tmp")
	for _, directory := range []string{copilotHome, temp} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatalf("create sandbox runtime directory: %v", err)
		}
	}
	outputPath := filepath.Join(workspace, "SANDBOX_AUTH_OK.txt")
	command := exec.Command(
		"copilot",
		"-p", "Create SANDBOX_AUTH_OK.txt in the current directory containing exactly: authenticated",
		"--allow-all-tools",
		"--log-dir", filepath.Join(workspace, ".logs"),
		"--log-level", "error",
	)
	command.Dir = workspace
	command.Env = liveCopilotEnv(copilotHome, temp)
	if err := s.Wrap(command, Policy{Workspace: workspace}); err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("sandboxed copilot: %v\n%s", err, output)
	}

	content, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read Copilot output: %v", err)
	}
	if strings.TrimSpace(string(content)) != "authenticated" {
		t.Fatalf("Copilot output = %q, want %q", content, "authenticated")
	}
	sessionState := filepath.Join(copilotHome, "session-state")
	if info, err := os.Stat(sessionState); err != nil {
		t.Fatalf("stat isolated Copilot session state: %v", err)
	} else if !info.IsDir() {
		t.Fatalf("isolated Copilot session state %q is not a directory", sessionState)
	}
}

func liveCopilotEnv(copilotHome, temp string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"COPILOT_HOME=" + copilotHome,
		"TMPDIR=" + temp,
	}
}
