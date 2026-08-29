package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/mcpio"
)

func TestWithAutoGoobersIOClaudeNoOpsWithoutSelfBin(t *testing.T) {
	workspace := t.TempDir()
	req := RunRequest{Envelope: testEnvelope(workspace), Workspace: workspace}
	req.Envelope.Inputs = map[string]interface{}{InputArtifactFile: "out.md"}

	got := withAutoGoobersIOClaude(req, "")
	if got.GoobersIORegistered {
		t.Fatal("must not register goobers-io without a known self-binary path")
	}
}

func TestWithAutoGoobersIOClaudeNoOpsWithoutRunIdentity(t *testing.T) {
	workspace := t.TempDir()
	req := RunRequest{Workspace: workspace, Tools: []string{"shell"}}

	got := withAutoGoobersIOClaude(req, "/usr/local/bin/goobers")
	if got.GoobersIORegistered {
		t.Fatal("a task without run identity must not be marked registered")
	}
}

// TestWithAutoGoobersIOClaudeNeverTouchesToolsOrMCPServers pins the finding
// behind claude_mcp_io.go's design: confirmed live that claude-code's
// --tools/--allowedTools don't gate MCP-server tools at all, so unlike
// Copilot's withAutoGoobersIO, the claude-code equivalent must never mutate
// req.Tools — doing so would flip every eligible run into claudeExtraArgs's
// tool-constrained path even for goobers that declare no Spec.Tools.
func TestWithAutoGoobersIOClaudeNeverTouchesToolsOrMCPServers(t *testing.T) {
	workspace := t.TempDir()
	req := RunRequest{
		Envelope:  testEnvelope(workspace),
		Workspace: workspace,
		Tools:     []string{"shell"},
	}
	req.Envelope.Inputs = map[string]interface{}{InputArtifactFile: "out.md"}

	got := withAutoGoobersIOClaude(req, "/usr/local/bin/goobers")
	if !got.GoobersIORegistered {
		t.Fatal("expected GoobersIORegistered once eligible with a known self-binary path")
	}
	if len(got.Tools) != 1 || got.Tools[0] != "shell" {
		t.Fatalf("Tools must be untouched, got %v", got.Tools)
	}
	if len(got.MCPServers) != 0 {
		t.Fatalf("must never populate MCPServers, got %v", got.MCPServers)
	}

	// Also confirm the empty-declared-tools case: registering goobers-io must
	// not manufacture a non-empty Tools list out of nothing.
	bare := RunRequest{Envelope: testEnvelope(workspace), Workspace: workspace}
	bare.Envelope.Inputs = map[string]interface{}{InputArtifactFile: "out.md"}
	got = withAutoGoobersIOClaude(bare, "/usr/local/bin/goobers")
	if len(got.Tools) != 0 {
		t.Fatalf("Tools must stay empty, got %v", got.Tools)
	}
}

func TestGoobersIOClaudeMCPConfigArgEmptyWithoutRunIdentity(t *testing.T) {
	workspace := t.TempDir()
	req := RunRequest{Workspace: workspace}

	arg, err := goobersIOClaudeMCPConfigArg(req, "/usr/local/bin/goobers")
	if err != nil {
		t.Fatal(err)
	}
	if arg != "" {
		t.Fatalf("expected no arg without run identity, got %q", arg)
	}
}

func TestGoobersIOClaudeMCPConfigArgEmptyWithoutSelfBin(t *testing.T) {
	workspace := t.TempDir()
	req := RunRequest{Envelope: testEnvelope(workspace), Workspace: workspace}
	req.Envelope.Inputs = map[string]interface{}{InputArtifactFile: "out.md"}

	arg, err := goobersIOClaudeMCPConfigArg(req, "")
	if err != nil {
		t.Fatal(err)
	}
	if arg != "" {
		t.Fatalf("expected no arg without a known self-binary path, got %q", arg)
	}
}

func TestGoobersIOClaudeMCPConfigArgBuildsRegistrationAndConfig(t *testing.T) {
	workspace := t.TempDir()
	req := RunRequest{
		Envelope:  testEnvelope(workspace),
		Workspace: workspace,
		ContextPaths: map[string]string{
			"review-code-quality.artifact[0]": ".goobers/context/00-review-code-quality.artifact_0_",
		},
	}
	req.Envelope.Inputs = map[string]interface{}{InputArtifactFile: "findings.md"}

	arg, err := goobersIOClaudeMCPConfigArg(req, "/usr/local/bin/goobers")
	if err != nil {
		t.Fatal(err)
	}
	if arg == "" {
		t.Fatal("expected a non-empty --mcp-config argument")
	}

	var parsed struct {
		MCPServers map[string]struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(arg), &parsed); err != nil {
		t.Fatalf("--mcp-config argument is not valid JSON: %v", err)
	}
	server, ok := parsed.MCPServers[goobersIOServerName]
	if !ok {
		t.Fatalf("registration missing %q server: %s", goobersIOServerName, arg)
	}
	// "stdio" is claude-code's own MCP transport vocabulary, confirmed live —
	// distinct from Copilot's "local", which claude-code's CLI doesn't
	// recognize.
	if server.Type != "stdio" || server.Command != "/usr/local/bin/goobers" {
		t.Fatalf("unexpected server registration: %+v", server)
	}
	if len(server.Args) != 3 || server.Args[0] != "mcp-io" || server.Args[1] != "--config" {
		t.Fatalf("unexpected server args: %v", server.Args)
	}
	configPath := server.Args[2]
	resolvedWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if rel, err := filepath.Rel(resolvedWorkspace, configPath); err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("config path %q is not inside the workspace %q", configPath, resolvedWorkspace)
	}

	cfg, err := mcpio.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("config file not written correctly: %v", err)
	}
	if cfg.Workspace != workspace {
		t.Errorf("Workspace = %q, want %q", cfg.Workspace, workspace)
	}
	if cfg.ArtifactFile != "findings.md" {
		t.Errorf("ArtifactFile = %q, want findings.md", cfg.ArtifactFile)
	}
	if got := cfg.Inputs["review-code-quality.artifact[0]"]; got != ".goobers/context/00-review-code-quality.artifact_0_" {
		t.Errorf("Inputs mapping = %q", got)
	}
	if cfg.RunID != "run-1" || cfg.WorkflowID != "default-implement" || cfg.TaskID != "implement" || cfg.Gaggle != "example" {
		t.Errorf("run identity = run %q, workflow %q, task %q, gaggle %q", cfg.RunID, cfg.WorkflowID, cfg.TaskID, cfg.Gaggle)
	}

	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config file mode = %o, want 600", info.Mode().Perm())
	}
}

// TestGoobersIOClaudeMCPConfigArgRefusesToTraverseAWorkspaceSymlink mirrors
// the Copilot regression test for the same #2408/#2413-class gap: this
// write happens in the harness's own trusted process, before the spawned
// claude subprocess is sandboxed, against a workspace that may contain
// repository-controlled content.
func TestGoobersIOClaudeMCPConfigArgRefusesToTraverseAWorkspaceSymlink(t *testing.T) {
	workspace := t.TempDir()
	outsideDir := t.TempDir()
	if err := os.Symlink(outsideDir, filepath.Join(workspace, ".goobers")); err != nil {
		t.Fatal(err)
	}

	req := RunRequest{
		Envelope:  testEnvelope(workspace),
		Workspace: workspace,
	}
	req.Envelope.Inputs = map[string]interface{}{InputArtifactFile: "findings.md"}

	if _, err := goobersIOClaudeMCPConfigArg(req, "/usr/local/bin/goobers"); err == nil {
		t.Fatal("expected goobersIOClaudeMCPConfigArg to refuse to traverse the symlinked .goobers directory")
	}
	if _, err := os.Lstat(filepath.Join(outsideDir, "mcp-io")); !os.IsNotExist(err) {
		t.Fatalf("runtime directory was created outside the workspace through the symlink: err=%v", err)
	}
}

// TestClaudeAdapterRunWiresGoobersIO is the Run()-level integration test:
// confirms --mcp-config/--strict-mcp-config land in the actual argv, the
// prompt carries the goobers-io section, and req.Tools stays exactly what
// the goober declared (here, nothing) — no --tools/--allowedTools flags
// appear, matching TestClaudeAdapterEmptyToolAllowlistPreservesCommand's
// no-Spec.Tools baseline plus goobers-io layered on top.
func TestClaudeAdapterRunWiresGoobersIO(t *testing.T) {
	stubClaudeCredentialsHome(t)
	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &ClaudeAdapter{
		Command: []string{"claude"},
		Runner:  runner,
		SelfBin: "/usr/local/bin/goobers",
	}
	envelope := testEnvelope(workspace)
	envelope.Inputs = map[string]interface{}{InputArtifactFile: "findings.md"}
	_, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       envelope,
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	command := runner.lastReq.Command

	if slices.Contains(command, "--tools") || slices.Contains(command, "--allowedTools") {
		t.Fatalf("goobers-io registration must not flip on tool-constrained mode: %v", command)
	}
	for _, want := range []string{"--permission-mode", "bypassPermissions"} {
		if !slices.Contains(command, want) {
			t.Errorf("command missing %q: %v", want, command)
		}
	}

	idx := slices.Index(command, "--mcp-config")
	if idx == -1 || idx+1 >= len(command) {
		t.Fatalf("command missing --mcp-config value: %v", command)
	}
	var parsed struct {
		MCPServers map[string]struct {
			Type string `json:"type"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(command[idx+1]), &parsed); err != nil {
		t.Fatalf("--mcp-config value is not valid JSON: %v", err)
	}
	if parsed.MCPServers[goobersIOServerName].Type != "stdio" {
		t.Fatalf("unexpected --mcp-config value: %s", command[idx+1])
	}
	if !slices.Contains(command, "--strict-mcp-config") {
		t.Errorf("command missing --strict-mcp-config: %v", command)
	}

	debugPrompt, err := os.ReadFile(filepath.Join(workspace, ".goobers", "prompt.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(debugPrompt), "## goobers-io tools") || !strings.Contains(string(debugPrompt), "publish_output") {
		t.Fatalf("prompt missing goobers-io section:\n%s", debugPrompt)
	}
}

// TestClaudeAdapterRunOmitsGoobersIOWithoutSelfBin confirms the adapter
// falls back to byte-identical pre-#2774 behavior when SelfBin is unset —
// the state every existing claude_test.go Run() test exercises.
func TestClaudeAdapterRunOmitsGoobersIOWithoutSelfBin(t *testing.T) {
	command := runClaudeAdapterForCommand(t, nil)
	if slices.Contains(command, "--mcp-config") || slices.Contains(command, "--strict-mcp-config") {
		t.Fatalf("must not wire goobers-io without a known self-binary path: %v", command)
	}
}

// TestClaudeAdapterReportsRegisteredMCPServersLostAtInvocation is the
// regression for #3356's product invariant: a stage whose resolved config
// wired the goobers-io MCP server ran with those tools silently absent, and
// nothing in the run named the loss. The claude CLI reports per-server
// connection state in its system/init event's mcp_servers field; the adapter
// must surface every registered-but-unconnected server in
// Outcome.MCPServerFailures instead of proceeding as if nothing happened —
// and must claim nothing when the CLI produced no report at all.
func TestClaudeAdapterReportsRegisteredMCPServersLostAtInvocation(t *testing.T) {
	const resultLine = `{"type":"result","subtype":"success","result":"done"}`
	for _, tc := range []struct {
		name string
		init string
		want []MCPServerFailure
	}{
		{
			name: "registered server missing from the CLI report",
			init: `{"type":"system","subtype":"init","model":"claude-sonnet-4-6","mcp_servers":[]}`,
			want: []MCPServerFailure{{Server: goobersIOServerName, Status: "absent"}},
		},
		{
			name: "registered server failed to connect",
			init: `{"type":"system","subtype":"init","model":"claude-sonnet-4-6","mcp_servers":[{"name":"goobers-io","status":"failed"}]}`,
			want: []MCPServerFailure{{Server: goobersIOServerName, Status: "failed"}},
		},
		{
			name: "connected server reports no failure",
			init: `{"type":"system","subtype":"init","model":"claude-sonnet-4-6","mcp_servers":[{"name":"goobers-io","status":"connected"}]}`,
			want: nil,
		},
		{
			name: "init without an mcp_servers field claims nothing",
			init: `{"type":"system","subtype":"init","model":"claude-sonnet-4-6"}`,
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubClaudeCredentialsHome(t)
			workspace := t.TempDir()
			stream := tc.init + "\n" + resultLine + "\n"
			runner := &fakeProcessRunner{
				result: ProcessResult{ExitCode: 0, Transcript: []byte(stream)},
				act: func(req ProcessRequest) error {
					return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{
						Status: apiv1.ResultSuccess,
					})
				},
			}
			adapter := &ClaudeAdapter{
				Command: []string{"claude"},
				Runner:  runner,
				SelfBin: "/usr/local/bin/goobers",
			}
			out, err := adapter.Run(context.Background(), RunRequest{
				Envelope:       testEnvelope(workspace),
				Workspace:      workspace,
				CompletionPath: DefaultResultPath,
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !slices.Equal(out.MCPServerFailures, tc.want) {
				t.Fatalf("MCPServerFailures = %+v, want %+v", out.MCPServerFailures, tc.want)
			}
		})
	}
}

// TestClaudeMCPServerFailuresCoversDeclaredServers confirms the comparison
// covers a goober's declared mcpServers alongside the auto-wired goobers-io
// registration, deduplicates, and never claims anything without an explicit
// CLI report (#3356).
func TestClaudeMCPServerFailuresCoversDeclaredServers(t *testing.T) {
	req := RunRequest{
		GoobersIORegistered: true,
		MCPServers:          []apiv1.MCPServer{{Name: "github"}, {Name: "github"}},
	}
	capture := transcriptCapture{
		mcpServersReported: true,
		mcpServerStatus:    map[string]string{goobersIOServerName: "connected"},
	}
	got := claudeMCPServerFailures(req, capture)
	want := []MCPServerFailure{{Server: "github", Status: "absent"}}
	if !slices.Equal(got, want) {
		t.Fatalf("failures = %+v, want %+v", got, want)
	}

	if got := claudeMCPServerFailures(req, transcriptCapture{}); got != nil {
		t.Fatalf("an unreported capture must claim nothing, got %+v", got)
	}
	if got := claudeMCPServerFailures(RunRequest{}, capture); got != nil {
		t.Fatalf("no registered servers must produce no failures, got %+v", got)
	}
}
