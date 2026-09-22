package harness

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareCopilotPreflightEnvironmentCopiesOnlyStoredAuthentication(t *testing.T) {
	ambientHome := t.TempDir()
	storedConfig := []byte(`{"oauthToken":"stored-login"}`)
	if err := os.WriteFile(filepath.Join(ambientHome, copilotConfigFileName), storedConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"mcp-config.json",
		"copilot-instructions.md",
		filepath.Join("hooks", "pre-tool.sh"),
		filepath.Join("extensions", "slow-extension", "extension.json"),
		filepath.Join("installed-plugins", "slow-plugin", "plugin.json"),
	} {
		fullPath := filepath.Join(ambientHome, path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte("ambient customization"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	env, dir, cleanup, err := prepareCopilotPreflightEnvironment(
		[]string{
			"COPILOT_HOME=" + ambientHome,
			copilotWorkspaceMCPEnv + "=true",
			copilotPluginDirOnlyEnv + "=false",
		},
		true,
	)
	if err != nil {
		t.Fatalf("prepareCopilotPreflightEnvironment: %v", err)
	}
	if dir == ambientHome {
		t.Fatal("preflight reused the ambient Copilot home")
	}
	isolatedHome, ok := copilotConfigHome(env)
	if !ok || isolatedHome != dir {
		t.Fatalf("isolated COPILOT_HOME = %q, %v; want %q", isolatedHome, ok, dir)
	}
	gotConfig, err := os.ReadFile(filepath.Join(dir, copilotConfigFileName))
	if err != nil {
		t.Fatalf("read seeded config: %v", err)
	}
	if string(gotConfig) != string(storedConfig) {
		t.Fatalf("seeded config = %q, want %q", gotConfig, storedConfig)
	}
	for _, path := range []string{
		"mcp-config.json",
		"copilot-instructions.md",
		"hooks",
		"extensions",
		"installed-plugins",
	} {
		if _, err := os.Stat(filepath.Join(dir, path)); !os.IsNotExist(err) {
			t.Fatalf("ambient customization %q reached isolated home: %v", path, err)
		}
	}
	if value, ok := environmentValue(env, copilotWorkspaceMCPEnv); ok {
		t.Fatalf("%s remained enabled with value %q", copilotWorkspaceMCPEnv, value)
	}
	if value, ok := environmentValue(env, copilotPluginDirOnlyEnv); !ok || value != "true" {
		t.Fatalf("%s = %q, %v; want true", copilotPluginDirOnlyEnv, value, ok)
	}

	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("isolated preflight home was not removed: %v", err)
	}
}

func TestPrepareCopilotPreflightEnvironmentSkipsStoredConfigWithExplicitToken(t *testing.T) {
	ambientHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(ambientHome, copilotConfigFileName), []byte(`{"oauthToken":"ambient"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, dir, cleanup, err := prepareCopilotPreflightEnvironment(
		[]string{"COPILOT_HOME=" + ambientHome, "COPILOT_GITHUB_TOKEN=explicit"},
		false,
	)
	if err != nil {
		t.Fatalf("prepareCopilotPreflightEnvironment: %v", err)
	}
	defer cleanup()
	if _, err := os.Stat(filepath.Join(dir, copilotConfigFileName)); !os.IsNotExist(err) {
		t.Fatalf("stored config copied despite explicit token: %v", err)
	}
}

func TestPreflightProbeErrorReportsTimeoutBeforeExitCode(t *testing.T) {
	err := preflightProbeError(
		`harness: copilot-cli: "copilot" (sign-in check)`,
		ProcessResult{ExitCode: 1, Transcript: []byte("AgenticDeck canvas ready")},
		errors.Join(ErrTimeout, errors.New("after 1m30s")),
		"sign in",
	)
	message := err.Error()
	if !strings.Contains(message, "timed out") {
		t.Fatalf("timeout was masked: %v", err)
	}
	if strings.Contains(message, "exited 1") {
		t.Fatalf("timeout was misclassified as an exit failure: %v", err)
	}
}
