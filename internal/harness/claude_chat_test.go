package harness

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/chat"
	claudechat "github.com/goobers/goobers/internal/chat/claude"
)

func TestClaudeChatPreservesHardening(t *testing.T) {
	stubClaudeCredentialsHome(t)
	for _, sandboxed := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "sandbox"}[sandboxed], func(t *testing.T) {
			workspace := t.TempDir()
			adapter := &ClaudeAdapter{Command: []string{"claude"}, KeychainReader: absentClaudeKeychain}
			req := RunRequest{Envelope: testEnvelope(workspace), Workspace: workspace, Tools: []string{"shell"}}
			if sandboxed {
				req.Sandbox = &stubSandbox{}
			}
			o := chat.StartOptions{Workspace: workspace, Level: chat.FullAccess, Ceiling: chat.NewCeiling(chat.Ask, chat.FullAccess, chat.FullAccess), SystemPrompt: "secret system prompt", ResumeNativeID: "resume-id"}
			d := adapter.ChatDriver(req)
			launch, err := d.Prepare(context.Background(), o, claudechat.Args(o))
			if err != nil {
				t.Fatal(err)
			}
			if launch.Cleanup != nil {
				defer func() { _ = launch.Cleanup() }()
			}
			if !containsRun(launch.Command, wantClaudeHostSafetyArgs) {
				t.Fatal("name-kill denies missing")
			}
			if !slices.Contains(launch.Command, "--strict-mcp-config") {
				t.Fatal("ambient MCP allowed")
			}
			if commandOptionValue(launch.Command, "--permission-mode") != "default" {
				t.Fatal("ceiling not clamped")
			}
			if commandOptionValue(launch.Command, "--resume") != "resume-id" {
				t.Fatal("resume missing")
			}
			if !strings.Contains(commandOptionValue(launch.Command, "--tools"), "Bash") {
				t.Fatal("declared tools missing")
			}
			if slices.Contains(launch.Command, "--allowedTools") {
				t.Fatal("interactive permissions pre-approved")
			}
			if sandboxed && launch.Command[0] != "sandbox-stub" {
				t.Fatal("sandbox wrapper lost")
			}
			if strings.Contains(strings.Join(launch.Command, " "), o.SystemPrompt) {
				t.Fatal("prompt in argv")
			}
			wantDir := filepath.Join(workspace, ".goobers", "sandbox", "claude-config")
			if !slices.Contains(launch.Env, "CLAUDE_CONFIG_DIR="+wantDir) {
				t.Fatal("ambient config inherited")
			}
			info, err := os.Stat(wantDir)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm()&0077 != 0 {
				t.Fatal("config not private")
			}
		})
	}
}
func TestClaudeChatRejectsWorkspaceChange(t *testing.T) {
	adapter := &ClaudeAdapter{Command: []string{"claude"}, KeychainReader: absentClaudeKeychain}
	req := RunRequest{Workspace: t.TempDir()}
	_, err := adapter.ChatDriver(req).Prepare(context.Background(), chat.StartOptions{Workspace: t.TempDir()}, nil)
	if err == nil {
		t.Fatal("changed workspace accepted")
	}
}
func TestClaudeChatCredentialGuard(t *testing.T) {
	stubClaudeCredentialsHome(t)
	adapter := &ClaudeAdapter{Command: []string{"claude"}}
	req := RunRequest{Workspace: t.TempDir()}
	defer func() {
		if recover() == nil {
			t.Fatal("missing test keychain reader did not trip credential guard")
		}
	}()
	_, _ = adapter.ChatDriver(req).Prepare(context.Background(), chat.StartOptions{}, claudechat.Args(chat.StartOptions{}))
}

func TestClaudeChatRejectsNestedBypass(t *testing.T) {
	adapter := &ClaudeAdapter{Command: []string{"claude"}, KeychainReader: absentClaudeKeychain}
	req := RunRequest{Workspace: t.TempDir(), ExecutionPolicy: &apiv1.ChildExecutionPolicy{}}
	_, err := adapter.ChatDriver(req).Prepare(context.Background(), chat.StartOptions{}, nil)
	if err == nil {
		t.Fatal("nested policy bypass accepted")
	}
}
