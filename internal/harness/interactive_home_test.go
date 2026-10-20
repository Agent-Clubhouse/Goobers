package harness

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/goobers/goobers/internal/chat"
	claudechat "github.com/goobers/goobers/internal/chat/claude"
)

func TestNativeChatProvidersIsolateHomeAndWrapProcess(t *testing.T) {
	stubClaudeCredentialsHome(t)
	for _, provider := range []string{"copilot", "claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			workspace := t.TempDir()
			boundary := &stubSandbox{}
			req := RunRequest{Workspace: workspace, IsolateHome: true, Sandbox: boundary}
			var command, env []string
			switch provider {
			case "copilot":
				runner := &interactiveCaptureRunner{}
				adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}
				_, _ = adapter.OpenInteractive(t.Context(), req)
				command, env = runner.request.Command, runner.request.Env
			case "claude":
				adapter := &ClaudeAdapter{Command: []string{"claude"}, KeychainReader: absentClaudeKeychain}
				options := chat.StartOptions{Workspace: workspace}
				launch, err := adapter.ChatDriver(req).Prepare(t.Context(), options, claudechat.Args(options))
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = launch.Cleanup() }()
				command, env = launch.Command, launch.Env
			case "codex":
				adapter := &CodexAdapter{Command: []string{"codex"}, ModelCredential: func(context.Context) (string, error) { return "sk-fixture-only", nil }}
				prepared, err := prepareCodexChat(t.Context(), CodexChatConfig{Adapter: adapter, Request: req}, chat.StartOptions{})
				if err != nil {
					t.Fatal(err)
				}
				defer prepared.cleanup()
				command, env = prepared.argv, prepared.env
			}
			if len(command) == 0 || command[0] != "sandbox-stub" || len(boundary.policies) == 0 {
				t.Fatal("native process escaped the OS wrapper")
			}
			home := filepath.Join(workspace, sandboxRuntimeSubdir, "home")
			if !slices.Contains(env, "HOME="+home) || slices.Contains(env, "HOME="+os.Getenv("HOME")) {
				t.Fatal("native process retained the ambient home")
			}
			if info, err := os.Stat(home); err != nil || info.Mode().Perm()&0077 != 0 {
				t.Fatal("runtime home is not private")
			}
		})
	}
}

func TestInteractiveHomeFailsClosedWithoutSandbox(t *testing.T) {
	if _, err := isolateInteractiveHome(RunRequest{Workspace: t.TempDir(), IsolateHome: true}, nil); err == nil {
		t.Fatal("isolated home silently downgraded the sandbox requirement")
	}
}
