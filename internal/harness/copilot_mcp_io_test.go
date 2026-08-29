package harness

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/mcpconfig"
	"github.com/goobers/goobers/internal/mcpio"
)

func TestAutoGoobersIOEligible(t *testing.T) {
	workspace := t.TempDir()
	bare := RunRequest{Envelope: testEnvelope(workspace), Workspace: workspace}
	if !autoGoobersIOEligible(bare) {
		t.Fatal("a valid invocation must be eligible for get_run_info")
	}

	withArtifact := bare
	withArtifact.Envelope.Inputs = map[string]interface{}{InputArtifactFile: "out.md"}
	if !autoGoobersIOEligible(withArtifact) {
		t.Fatal("a declared artifactFile must make a task eligible")
	}

	withContext := bare
	withContext.ContextPaths = map[string]string{"x": "y"}
	if !autoGoobersIOEligible(withContext) {
		t.Fatal("non-empty ContextPaths must make a task eligible")
	}
}

func TestWithAutoGoobersIONoOpsWithoutSelfBin(t *testing.T) {
	workspace := t.TempDir()
	req := RunRequest{Envelope: testEnvelope(workspace), Workspace: workspace}
	req.Envelope.Inputs = map[string]interface{}{InputArtifactFile: "out.md"}

	got := withAutoGoobersIO(req, "")
	if len(got.Tools) != 0 {
		t.Fatalf("must not grant tools without a known self-binary path, got %v", got.Tools)
	}
}

func TestWithAutoGoobersIONoOpsWithoutRunIdentity(t *testing.T) {
	workspace := t.TempDir()
	req := RunRequest{Workspace: workspace, Tools: []string{"shell"}}

	got := withAutoGoobersIO(req, "/usr/local/bin/goobers")
	if len(got.Tools) != 1 || got.Tools[0] != "shell" {
		t.Fatalf("a task without run identity must be returned unchanged, got Tools=%v", got.Tools)
	}
}

func TestWithAutoGoobersIOGrantsToolsButNeverTouchesMCPServers(t *testing.T) {
	workspace := t.TempDir()
	req := RunRequest{
		Envelope:  testEnvelope(workspace),
		Workspace: workspace,
		Tools:     []string{"shell"},
	}
	req.Envelope.Inputs = map[string]interface{}{InputArtifactFile: "out.md"}

	got := withAutoGoobersIO(req, "/usr/local/bin/goobers")
	// req.Tools feeds --available-tools=, which needs the server-prefixed
	// form for an externally-registered server to resolve at all (confirmed
	// live) — not the bare names the server's own "tools" field uses.
	wantTools := []string{"shell", "goobers-io-get_run_info", "goobers-io-publish_output", "goobers-io-list_inputs", "goobers-io-read_input", "goobers-io-grep_input"}
	if len(got.Tools) != len(wantTools) {
		t.Fatalf("Tools = %v, want %v", got.Tools, wantTools)
	}
	for i, want := range wantTools {
		if got.Tools[i] != want {
			t.Errorf("Tools[%d] = %q, want %q", i, got.Tools[i], want)
		}
	}
	// The whole point of the redesign (#2406 review): goobers-io must never
	// appear in req.MCPServers, or it re-triggers internal/mcpconfig's
	// credential-isolation check and requireMCPModelCredential against a
	// server that has neither credentials nor any need for the model auth
	// machinery those checks protect.
	if len(got.MCPServers) != 0 {
		t.Fatalf("withAutoGoobersIO must never populate MCPServers, got %v", got.MCPServers)
	}
}

func TestGoobersIOAdditionalMCPConfigArgEmptyWithoutRunIdentity(t *testing.T) {
	workspace := t.TempDir()
	req := RunRequest{Workspace: workspace}

	arg, err := goobersIOAdditionalMCPConfigArg(req, "/usr/local/bin/goobers")
	if err != nil {
		t.Fatal(err)
	}
	if arg != "" {
		t.Fatalf("expected no arg without run identity, got %q", arg)
	}
}

func TestGoobersIOAdditionalMCPConfigArgEmptyWithoutSelfBin(t *testing.T) {
	workspace := t.TempDir()
	req := RunRequest{Envelope: testEnvelope(workspace), Workspace: workspace}
	req.Envelope.Inputs = map[string]interface{}{InputArtifactFile: "out.md"}

	arg, err := goobersIOAdditionalMCPConfigArg(req, "")
	if err != nil {
		t.Fatal(err)
	}
	if arg != "" {
		t.Fatalf("expected no arg without a known self-binary path, got %q", arg)
	}
}

func TestGoobersIOAdditionalMCPConfigArgBuildsRegistrationAndConfig(t *testing.T) {
	workspace := t.TempDir()
	staleReceiptPath := filepath.Join(workspace, goobersIOReceiptFile())
	if err := os.MkdirAll(filepath.Dir(staleReceiptPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staleReceiptPath, []byte(`{"tool":"list_inputs","success":true}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	req := RunRequest{
		Envelope:  testEnvelope(workspace),
		Workspace: workspace,
		ContextPaths: map[string]string{
			"review-code-quality.artifact[0]": ".goobers/context/00-review-code-quality.artifact_0_",
		},
	}
	req.Envelope.Inputs = map[string]interface{}{InputArtifactFile: "findings.md"}

	arg, err := goobersIOAdditionalMCPConfigArg(req, "/usr/local/bin/goobers")
	if err != nil {
		t.Fatal(err)
	}
	if arg == "" {
		t.Fatal("expected a non-empty --additional-mcp-config argument")
	}
	if _, err := os.Stat(staleReceiptPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale receipt log was not reset: %v", err)
	}
	if strings.HasPrefix(arg, "{") {
		t.Fatalf("--additional-mcp-config must be a file path, got inline JSON %q", arg)
	}
	registrationData, err := os.ReadFile(arg)
	if err != nil {
		t.Fatalf("read --additional-mcp-config file: %v", err)
	}

	var parsed struct {
		MCPServers map[string]struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Args    []string `json:"args"`
			Tools   []string `json:"tools"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(registrationData, &parsed); err != nil {
		t.Fatalf("--additional-mcp-config file is not valid JSON: %v", err)
	}
	server, ok := parsed.MCPServers[goobersIOServerName]
	if !ok {
		t.Fatalf("registration missing %q server: %s", goobersIOServerName, registrationData)
	}
	if server.Type != "local" || server.Command != "/usr/local/bin/goobers" {
		t.Fatalf("unexpected server registration: %+v", server)
	}
	if len(server.Args) != 3 || server.Args[0] != "mcp-io" || server.Args[1] != "--config" {
		t.Fatalf("unexpected server args: %v", server.Args)
	}
	configPath := server.Args[2]
	// Must be workspace-relative, never $COPILOT_HOME-relative — that's the
	// whole point of the redesign (stored-login auth must not require
	// COPILOT_HOME redirection for goobers-io to work). resolveRooted
	// canonicalizes the workspace root itself before joining (a #2408 review
	// finding), so on a platform where t.TempDir() is itself reached through
	// a symlink (e.g. macOS's /var -> /private/var), configPath is resolved
	// too — compare against the same resolved form rather than the raw
	// workspace string.
	resolvedWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if rel, err := filepath.Rel(resolvedWorkspace, arg); err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("registration path %q is not inside the workspace %q", arg, resolvedWorkspace)
	}
	if rel, err := filepath.Rel(resolvedWorkspace, configPath); err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("config path %q is not inside the workspace %q", configPath, resolvedWorkspace)
	}

	wantTools := []string{"get_run_info", "publish_output", "list_inputs", "read_input", "grep_input"}
	if len(server.Tools) != len(wantTools) {
		t.Fatalf("server.Tools = %v, want %v", server.Tools, wantTools)
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
	if cfg.ReceiptFile != goobersIOReceiptFile() {
		t.Errorf("ReceiptFile = %q, want %q", cfg.ReceiptFile, goobersIOReceiptFile())
	}
	if got := cfg.Inputs["review-code-quality.artifact[0]"]; got != ".goobers/context/00-review-code-quality.artifact_0_" {
		t.Errorf("Inputs mapping = %q", got)
	}
	if cfg.RunID != "run-1" || cfg.WorkflowID != "default-implement" || cfg.TaskID != "implement" || cfg.Gaggle != "example" {
		t.Errorf("run identity = run %q, workflow %q, task %q, gaggle %q", cfg.RunID, cfg.WorkflowID, cfg.TaskID, cfg.Gaggle)
	}

	// The file must be private (0600). Unix mode bits are meaningless on
	// NTFS — os.WriteFile's mode argument only toggles the read-only
	// attribute there, so a 0600 request surfaces back as 0666 (see
	// internal/platform/secfile's doc comment, which is why that package
	// verifies privacy via the ACL/DACL instead of Perm() on Windows). This
	// config file isn't routed through secfile (no ambient credential to
	// protect, per goobersIOAdditionalMCPConfigArg's doc comment), so there's
	// nothing meaningful to assert here on Windows.
	if runtime.GOOS == "windows" {
		return
	}
	for _, path := range []string{arg, configPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("config file %q mode = %o, want 600", path, info.Mode().Perm())
		}
	}
}

// TestGoobersIOAdditionalMCPConfigArgRefusesToTraverseAWorkspaceSymlink is a
// regression test for a #2408 review finding: goobersIOAdditionalMCPConfigArg
// writes its runtime config in the harness's own process, before the spawned
// copilot subprocess is sandboxed — req.Workspace is the task's own
// worktree, which may contain repository-controlled content. A symlink at
// goobersIORuntimeSubdir (or, as here, an ancestor of it — a repository
// could equally plant one at goobersIORuntimeSubdir itself, or the config
// file's exact leaf path) pointing outside the workspace must not be
// followed by this write. Proves both that the call fails and that nothing
// is created outside the workspace.
func TestGoobersIOAdditionalMCPConfigArgRefusesToTraverseAWorkspaceSymlink(t *testing.T) {
	workspace := t.TempDir()
	outsideDir := t.TempDir()
	// goobersIORuntimeSubdir is ".goobers/mcp-io" — plant the symlink at
	// ".goobers" itself, so both the runtime dir and the config file inside
	// it are reached only by traversing it.
	if err := os.Symlink(outsideDir, filepath.Join(workspace, ".goobers")); err != nil {
		t.Fatal(err)
	}

	req := RunRequest{
		Envelope:  testEnvelope(workspace),
		Workspace: workspace,
	}
	req.Envelope.Inputs = map[string]interface{}{InputArtifactFile: "findings.md"}

	if _, err := goobersIOAdditionalMCPConfigArg(req, "/usr/local/bin/goobers"); err == nil {
		t.Fatal("expected goobersIOAdditionalMCPConfigArg to refuse to traverse the symlinked .goobers directory")
	}

	if _, err := os.Lstat(filepath.Join(outsideDir, "mcp-io")); !os.IsNotExist(err) {
		t.Fatalf("runtime directory was created outside the workspace through the symlink: err=%v", err)
	}
}

// TestAutoWiringDoesNotTriggerModelCredentialRequirement is a direct
// regression test for review finding #1: before this redesign, a
// goobers-io-eligible stage with no other declared MCP server would gain
// one via auto-wiring, tripping requireMCPModelCredential's
// `len(req.MCPServers) > 0` gate and rejecting runs that rely on Copilot's
// documented stored-CLI-login auth (no explicit agent:model credential
// configured at all). Confirmed live during #2406 development. Since
// withAutoGoobersIO no longer touches MCPServers, that gate now stays
// closed for exactly this case.
func TestAutoWiringDoesNotTriggerModelCredentialRequirement(t *testing.T) {
	workspace := t.TempDir()
	req := RunRequest{Envelope: testEnvelope(workspace), Workspace: workspace}
	req.Envelope.Inputs = map[string]interface{}{InputArtifactFile: "out.md"}

	got := withAutoGoobersIO(req, "/usr/local/bin/goobers")
	if len(got.MCPServers) != 0 {
		t.Fatalf("requireMCPModelCredential's len(req.MCPServers) > 0 gate would now fire; MCPServers = %v", got.MCPServers)
	}
}

// TestAutoWiringDoesNotBreakCredentialedLocalMCPServer is a direct
// regression test for review finding #2: before this redesign, auto-wiring
// added goobers-io (a local stdio server with no CredentialRefs) to
// req.MCPServers unconditionally. internal/mcpconfig's
// validateCopilotCredentialIsolation requires every local server to declare
// every credential any other local server in the same invocation has,
// because Copilot's local servers share one process environment — so a
// goober that separately declared its own real, credentialed local MCP
// server would fail configuration validation the moment it also became
// goobers-io-eligible, even though that server was valid on its own.
func TestAutoWiringDoesNotBreakCredentialedLocalMCPServer(t *testing.T) {
	workspace := t.TempDir()
	credentialedServer := apiv1.MCPServer{
		Name:    "vendor-tool",
		Command: "vendor-mcp-server",
		CredentialRefs: []apiv1.MCPCredentialRef{{
			Kind: apiv1.MCPCredentialKindBYO,
			Ref:  "vendor-api",
			Env:  "VENDOR_TOKEN",
		}},
	}
	req := RunRequest{
		Envelope:    testEnvelope(workspace),
		Workspace:   workspace,
		MCPServers:  []apiv1.MCPServer{credentialedServer},
		Credentials: mcpTestCredentials(t, mcpconfig.BYOCredentialKey("vendor-api"), "vendor-secret"),
	}
	req.Envelope.Inputs = map[string]interface{}{InputArtifactFile: "out.md"}

	got := withAutoGoobersIO(req, "/usr/local/bin/goobers")
	if len(got.MCPServers) != 1 || got.MCPServers[0].Name != "vendor-tool" {
		t.Fatalf("expected only the genuinely declared server, got %+v", got.MCPServers)
	}
	// This is exactly what internal/mcpconfig.ValidateForHarness (called
	// from prepareCopilotMCP) runs during a real invocation — proving it
	// passes is the actual regression check, not just inspecting the slice.
	if err := mcpconfig.ValidateForHarness(apiv1.HarnessCopilot, got.MCPServers, nil, got.Tools); err != nil {
		t.Fatalf("credentialed local MCP server config no longer validates: %v", err)
	}

	// And goobers-io itself is still fully available, delivered
	// independently — the fix isn't "goobers-io silently stops working when
	// another local server is declared," it's "the two no longer interfere."
	env, err := prepareCopilotMCP(context.Background(), got, nil)
	if err != nil {
		t.Fatalf("prepareCopilotMCP for the genuine server: %v", err)
	}
	_ = env
	arg, err := goobersIOAdditionalMCPConfigArg(got, "/usr/local/bin/goobers")
	if err != nil {
		t.Fatal(err)
	}
	if arg == "" {
		t.Fatal("expected goobers-io to still be registered via --additional-mcp-config")
	}
}

func TestAppendMissingDedupesAndPreservesOrder(t *testing.T) {
	got := appendMissing([]string{"shell", "publish_output"}, "publish_output", "list_inputs")
	want := []string{"shell", "publish_output", "list_inputs"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestGoobersIOPromptSection(t *testing.T) {
	workspace := t.TempDir()
	base := testEnvelope(workspace)

	writeOnly := RunRequest{Envelope: base, Workspace: workspace}
	writeOnly.Envelope.Inputs = map[string]interface{}{InputArtifactFile: "out.md"}
	section := goobersIOPromptSection(writeOnly)
	if !strings.Contains(section, "publish_output") {
		t.Error("expected the publish_output directive when artifactFile is declared")
	}
	if strings.Contains(section, "grep_input") {
		t.Error("did not expect the read directive when there is no context")
	}

	readOnly := RunRequest{Envelope: base, Workspace: workspace, ContextPaths: map[string]string{"x": "y"}}
	section = goobersIOPromptSection(readOnly)
	if strings.Contains(section, "publish_output") {
		t.Error("did not expect the publish_output directive when no artifactFile is declared")
	}
	if !strings.Contains(section, "grep_input") || !strings.Contains(section, "read_input") {
		t.Error("expected the read directive when there is upstream context")
	}
}

// TestRenderPromptIncludesGoobersIOSectionOnlyWhenRegistered pins #2774's
// gating fix: the prompt section is keyed off req.GoobersIORegistered (set
// by an adapter only once it has actually wired the MCP server), not off
// eligibility alone (req.Envelope.RunID != ""). Before #2774, a valid
// invocation on any adapter that hadn't wired goobers-io — claude-code, at
// the time — still got instructed to call tools that didn't exist there.
func TestRenderPromptIncludesGoobersIOSectionOnlyWhenRegistered(t *testing.T) {
	workspace := t.TempDir()
	req := RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: "result.json",
	}
	if strings.Contains(renderPrompt(req), "## goobers-io tools") {
		t.Fatal("must not include the goobers-io section when the adapter never registered the server")
	}

	req.GoobersIORegistered = true
	if !strings.Contains(renderPrompt(req), "## goobers-io tools") {
		t.Fatal("must include the goobers-io section once the adapter registered the server")
	}

	req.Envelope.Inputs = map[string]interface{}{InputArtifactFile: "out.md"}
	rendered := renderPrompt(req)
	if !strings.Contains(rendered, "## goobers-io tools") || !strings.Contains(rendered, "publish_output") {
		t.Fatalf("expected the goobers-io section once eligible and registered, got:\n%s", rendered)
	}
}
