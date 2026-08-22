package harness

import (
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// Real line shapes, copied from a Copilot CLI 1.0.80 run log captured on the
// deployed instance (2026-08-21). Keeping them verbatim is the point: the
// parser exists only to read what this CLI actually writes, so a synthetic
// approximation would not defend the behavior.
const (
	copilotLogHandshakeGoobersIO = `2026-08-21T23:27:46.883Z [INFO] [rust:rmcp::service] Service initialized as client {"peer_info":"Some(ServerPeerInfo { protocol_version: ProtocolVersion(\"2025-11-25\"), capabilities: ServerCapabilities { experimental: None, extensions: None, logging: None, completions: None, prompts: None, resources: None, tools: Some(ToolsCapability { list_changed: None }) }, server_info: Some(Implementation { name: \"goobers-io\", title: None, version: \"1\", description: None, icons: None, website_url: None }), instructions: None, meta: None })"}`

	copilotLogHandshakeGitHub = `2026-08-21T23:27:47.266Z [INFO] [rust:rmcp::service] Service initialized as client {"peer_info":"Some(ServerPeerInfo { protocol_version: ProtocolVersion(\"2025-11-25\"), capabilities: ServerCapabilities { tools: Some(ToolsCapability { list_changed: None }) }, server_info: Some(Implementation { name: \"github-mcp-server\", title: Some(\"GitHub MCP Server\"), version: \"remote-66a8ec9\", description: None, icons: None, website_url: None }), instructions: None, meta: None })"}`

	copilotLogServerStderr = `2026-08-21T23:27:46.883Z [DEBUG] [rust:copilot_runtime::mcp::native_connector] MCP server stderr {"server_name":"goobers-io","stderr":"mcpio: MCP protocol version negotiated: requested=2025-11-25 agreed=2025-11-25"}`

	copilotLogPolicyCheck = `2026-08-21T23:27:46.279Z [DEBUG] [rust:copilot_runtime::github_telemetry::service] Sending telemetry event: cli.telemetry (kind: mcp_policy_check)`

	copilotLogNoMCP = `2026-08-21T23:27:45.272Z [INFO] [managedSettings] effective policy resolved: source=none, bypassDisabled=false, serverFetchFailed=false`
)

func writeCopilotLog(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	body := ""
	for _, line := range lines {
		body += line + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "process-1787354864694-732423.log"), []byte(body), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	return dir
}

func goobersIORequest() RunRequest {
	return RunRequest{GoobersIORegistered: true}
}

func TestCopilotMCPServerFailuresConnectedServerIsNotAFailure(t *testing.T) {
	dir := writeCopilotLog(t, copilotLogPolicyCheck, copilotLogServerStderr, copilotLogHandshakeGoobersIO)
	if got := copilotMCPServerFailures(goobersIORequest(), dir); len(got) != 0 {
		t.Fatalf("failures = %+v, want none for a server that completed its handshake", got)
	}
}

// The launch-failure case: registered, but the CLI's log never mentions it.
// This is what a missing or non-executable server binary looks like.
func TestCopilotMCPServerFailuresAbsentWhenNeverMentioned(t *testing.T) {
	dir := writeCopilotLog(t, copilotLogPolicyCheck, copilotLogHandshakeGitHub)
	got := copilotMCPServerFailures(goobersIORequest(), dir)
	if len(got) != 1 {
		t.Fatalf("failures = %+v, want exactly one", got)
	}
	if got[0].Server != goobersIOServerName || got[0].Status != copilotMCPStatusAbsent {
		t.Fatalf("failure = %+v, want %s/%s", got[0], goobersIOServerName, copilotMCPStatusAbsent)
	}
}

// The protocol-rejection case, and the one that actually happened: the server
// process started and spoke, but no handshake ever completed. Distinguishing
// it from "absent" is the difference between "your binary is missing" and
// "your binary was rejected", which are unrelated fixes.
func TestCopilotMCPServerFailuresStartedButNoHandshake(t *testing.T) {
	dir := writeCopilotLog(t, copilotLogPolicyCheck, copilotLogServerStderr)
	got := copilotMCPServerFailures(goobersIORequest(), dir)
	if len(got) != 1 {
		t.Fatalf("failures = %+v, want exactly one", got)
	}
	if got[0].Status != copilotMCPStatusHandshakeIncomplete {
		t.Fatalf("status = %q, want %q", got[0].Status, copilotMCPStatusHandshakeIncomplete)
	}
}

// Absence of evidence must never become evidence of absence: a log with no MCP
// activity at all (CLI died during auth, argument parsing, etc.) makes no claim.
func TestCopilotMCPServerFailuresSilentWithoutMCPActivity(t *testing.T) {
	dir := writeCopilotLog(t, copilotLogNoMCP)
	if got := copilotMCPServerFailures(goobersIORequest(), dir); got != nil {
		t.Fatalf("failures = %+v, want nil when the log shows no MCP activity", got)
	}
}

func TestCopilotMCPServerFailuresSilentWithoutLogDir(t *testing.T) {
	if got := copilotMCPServerFailures(goobersIORequest(), ""); got != nil {
		t.Fatalf("failures = %+v, want nil without a log directory", got)
	}
	if got := copilotMCPServerFailures(goobersIORequest(), filepath.Join(t.TempDir(), "missing")); got != nil {
		t.Fatalf("failures = %+v, want nil for an unreadable log directory", got)
	}
}

func TestCopilotMCPServerFailuresSilentWithoutRegisteredServers(t *testing.T) {
	dir := writeCopilotLog(t, copilotLogPolicyCheck)
	if got := copilotMCPServerFailures(RunRequest{}, dir); got != nil {
		t.Fatalf("failures = %+v, want nil when nothing was registered", got)
	}
}

// A declared server is judged alongside goobers-io, and each is reported with
// its own status rather than collapsing into a single verdict.
func TestCopilotMCPServerFailuresReportsEachRegisteredServer(t *testing.T) {
	req := RunRequest{
		GoobersIORegistered: true,
		MCPServers:          []apiv1.MCPServer{{Name: "github-mcp-server"}, {Name: "absent-server"}},
	}
	dir := writeCopilotLog(t, copilotLogPolicyCheck, copilotLogServerStderr, copilotLogHandshakeGitHub)
	got := copilotMCPServerFailures(req, dir)
	byServer := make(map[string]string, len(got))
	for _, failure := range got {
		byServer[failure.Server] = failure.Status
	}
	if _, ok := byServer["github-mcp-server"]; ok {
		t.Fatalf("failures = %+v, want no entry for the connected github server", got)
	}
	if byServer[goobersIOServerName] != copilotMCPStatusHandshakeIncomplete {
		t.Fatalf("goobers-io status = %q, want %q", byServer[goobersIOServerName], copilotMCPStatusHandshakeIncomplete)
	}
	if byServer["absent-server"] != copilotMCPStatusAbsent {
		t.Fatalf("absent-server status = %q, want %q", byServer["absent-server"], copilotMCPStatusAbsent)
	}
}
