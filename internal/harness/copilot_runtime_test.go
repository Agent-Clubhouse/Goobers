package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestCopilotCredentialedStageIsolatesAmbientStateThroughRepair(t *testing.T) {
	ambient := t.TempDir()
	t.Setenv("COPILOT_HOME", ambient)
	t.Setenv(copilotWorkspaceMCPEnv, "true")
	t.Setenv(copilotPluginDirOnlyEnv, "false")
	for _, name := range []string{"config.json", "mcp-config.json", "copilot-instructions.md", "hooks/pre-tool.sh", "installed-plugins/plugin.json"} {
		path := filepath.Join(ambient, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("ambient-login-and-customization-canary"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, confined := range []bool{false, true} {
		t.Run(map[bool]string{false: "unconfined", true: "sandboxed"}[confined], func(t *testing.T) {
			workspace := t.TempDir()
			var homes []string
			runner := &capturingProcessRunner{act: func(call int, process ProcessRequest) error {
				home, ok := environmentValue(process.Env, "COPILOT_HOME")
				if !ok || home == ambient || !strings.HasPrefix(home, workspace+string(os.PathSeparator)) {
					t.Fatalf("home is not invocation-scoped: %q", home)
				}
				homes = append(homes, home)
				if call%2 == 1 && homes[call-1] != home {
					t.Fatal("repair lost invocation home")
				}
				if call == 2 && homes[0] == home {
					t.Fatal("next invocation reused home")
				}
				for _, name := range []string{"config.json", "mcp-config.json", "copilot-instructions.md", "hooks", "installed-plugins"} {
					if _, err := os.Stat(filepath.Join(home, name)); !os.IsNotExist(err) {
						t.Fatalf("ambient %s imported: %v", name, err)
					}
				}
				if value, _ := environmentValue(process.Env, "COPILOT_GITHUB_TOKEN"); value != "scoped-model-token" {
					t.Fatal("model credential changed")
				}
				if _, ok := environmentValue(process.Env, copilotWorkspaceMCPEnv); ok {
					t.Fatal("workspace MCP enabled")
				}
				if value, _ := environmentValue(process.Env, copilotPluginDirOnlyEnv); value != "true" {
					t.Fatal("ambient plugins enabled")
				}
				if confined && process.Command[0] != "sandbox-stub" {
					t.Fatal("sandbox wrapper lost")
				}
				if call%2 == 0 {
					return nil
				}
				return WriteCompletion(process.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
			}}
			adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner, EnvCapabilities: map[string]string{"agent:model": "COPILOT_GITHUB_TOKEN"}, ExtraEnvAllowlist: []string{"COPILOT_HOME", copilotWorkspaceMCPEnv, copilotPluginDirOnlyEnv}}
			req := RunRequest{Envelope: testEnvelope(workspace, "agent:model"), Workspace: workspace, CompletionPath: DefaultResultPath, Credentials: mcpTestCredentials(t, "agent:model", "scoped-model-token")}
			if confined {
				req.Sandbox = &stubSandbox{}
			}
			for i := 0; i < 2; i++ {
				if i > 0 {
					if err := os.Remove(filepath.Join(workspace, DefaultResultPath)); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := adapter.Run(context.Background(), req); err != nil {
					t.Fatal(err)
				}
				if len(homes) != (i+1)*2 {
					t.Fatalf("calls=%d", len(homes))
				}
				if _, err := os.Stat(homes[i*2]); !os.IsNotExist(err) {
					t.Fatalf("runtime home survived: %v", err)
				}
			}
		})
	}
	if data, err := os.ReadFile(filepath.Join(ambient, "config.json")); err != nil || string(data) != "ambient-login-and-customization-canary" {
		t.Fatal("ambient login modified")
	}
}

func TestCopilotRuntimeDoesNotTreatRepositoryTokenAsModelCredential(t *testing.T) {
	adapter := &CopilotAdapter{}
	if adapter.hasCopilotModelCredential([]string{"GH_TOKEN=repository-token"}) {
		t.Fatal("repository token became model identity")
	}
	if !adapter.hasCopilotModelCredential([]string{"COPILOT_GITHUB_TOKEN=model-token"}) {
		t.Fatal("model token ignored")
	}
	adapter.EnvCapabilities = map[string]string{"agent:model": "CUSTOM_MODEL_TOKEN"}
	if !adapter.hasCopilotModelCredential([]string{"CUSTOM_MODEL_TOKEN=resolved-keychain-token"}) {
		t.Fatal("resolved model mapping ignored")
	}
}

func TestCopilotStageHomeRejectsSymlinkedRuntimeRoot(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, ".goobers"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, ".goobers", "copilot")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, _, err := prepareCopilotStageHome(workspace, nil); err == nil {
		t.Fatal("followed repository-controlled symlink")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatal("wrote outside workspace")
	}
}
