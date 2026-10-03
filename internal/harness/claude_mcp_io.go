package harness

import (
	"encoding/json"
	"fmt"
	"strings"
)

// withAutoGoobersIOClaude marks req eligible for the goobers-io MCP server
// when a self-binary path is known to launch it, adding only its exact MCP
// tool names to req.Tools so Claude admits and preapproves them.
// Sets GoobersIORegistered so the shared prompt renderer only mentions
// goobers-io tools once this adapter has actually registered them (#2774).
func goobersIOClaudeToolNames() []string {
	out := make([]string, len(goobersIOTools))
	for i, name := range goobersIOTools {
		out[i] = "mcp__" + goobersIOServerName + "__" + name
	}
	return out
}

func withAutoGoobersIOClaude(req RunRequest, selfBin string) RunRequest {
	if selfBin == "" || !autoGoobersIOEligible(req) {
		return req
	}
	req.Tools = appendMissing(req.Tools, goobersIOClaudeToolNames()...)
	req.GoobersIORegistered = true
	return req
}

// goobersIOClaudeMCPConfigArg builds the --mcp-config argument that
// registers goobers-io for this invocation, and writes its runtime config
// (workspace, declared artifactFile, materialized upstream inputs) the same
// way Copilot's goobersIOAdditionalMCPConfigArg does — see that function's
// doc comment for why mcpio.WriteConfig (not a plain os.MkdirAll+
// os.WriteFile) is load-bearing here. Returns ("", nil) when this invocation
// isn't eligible or selfBin is unknown.
//
// Claude's MCP server-type vocabulary is "stdio"/"sse"/"http", not
// Copilot's "local"/"http" — confirmed live via `claude mcp add-json --help`
// and a real --mcp-config run. Unlike Copilot's registration, no "tools"
// field is included: Claude's MCP config schema doesn't have a per-server
// tool sub-allowlist; its process-wide --tools/--allowedTools flags admit
// the exact names instead.
func goobersIOClaudeMCPConfigArg(req RunRequest, selfBin string) (string, error) {
	runtime, ok, err := prepareGoobersIOMCPRuntime(req, selfBin)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}

	server := struct {
		Type    string   `json:"type"`
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}{
		Type:    "stdio",
		Command: runtime.Command,
		Args:    runtime.Args,
	}
	data, err := json.Marshal(map[string]interface{}{
		"mcpServers": map[string]interface{}{goobersIOServerName: server},
	})
	if err != nil {
		return "", fmt.Errorf("encode goobers-io MCP registration: %w", err)
	}
	return string(data), nil
}

// claudeMCPServerFailures compares the MCP servers this invocation registered
// (goobers-io when GoobersIORegistered, plus every declared req.MCPServers
// entry) against the connection report the claude CLI emitted in its
// system/init event, and returns the ones that were not usable (#3356). A
// registered server whose subprocess fails to start is otherwise a fully
// silent loss: the CLI proceeds without its tools, the agent sees them as
// simply nonexistent, and the eventual stage failure (e.g. an agent-authored
// MISSING_REQUIRED_TOOLS block) says nothing about the cause. Returns nil
// when no init report was observed at all (CLI died before init, older CLI
// shape) — absence of the report is never treated as proof of absence of the
// servers.
func claudeMCPServerFailures(req RunRequest, capture transcriptCapture) []MCPServerFailure {
	if !capture.mcpServersReported {
		return nil
	}
	var registered []string
	if req.GoobersIORegistered {
		registered = append(registered, goobersIOServerName)
	}
	for _, server := range req.MCPServers {
		if server.Name != "" {
			registered = append(registered, server.Name)
		}
	}
	used := claudeMCPServersUsed(capture.mcpToolsUsed, registered)
	var failures []MCPServerFailure
	seen := make(map[string]struct{}, len(registered))
	for _, name := range registered {
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		status, ok := capture.mcpServerStatus[name]
		switch {
		case !ok:
			failures = append(failures, MCPServerFailure{Server: name, Status: "absent"})
		case status == claudeMCPStatusPending && used[name]:
			// The init report caught the server mid-handshake, and the turn
			// later called one of its tools successfully: it connected, just
			// after init (#5397). The same startup race the Copilot
			// pre-model probe waits out.
		case status == claudeMCPStatusPending:
			// Still mid-handshake at init and never proven usable: report it
			// as an incomplete handshake, a server fault that may clear on
			// the next attempt, rather than as the CLI's transient word.
			failures = append(failures, MCPServerFailure{Server: name, Status: copilotMCPStatusHandshakeIncomplete})
		case status != claudeMCPStatusConnected:
			failures = append(failures, MCPServerFailure{Server: name, Status: status})
		}
	}
	return failures
}

// claudeMCPStatusConnected is the status the claude CLI reports in its
// system/init mcp_servers entry for a server whose subprocess started and
// completed the MCP handshake.
const claudeMCPStatusConnected = "connected"

// claudeMCPStatusPending is the status the claude CLI reports in its
// system/init mcp_servers entry for a server whose handshake had not finished
// when init was emitted. The CLI keeps connecting it during the turn.
const claudeMCPStatusPending = "pending"

// claudeMCPToolUseTracker records the MCP tools a claude-code turn
// demonstrably reached: a tool_use naming an MCP tool (mcp__<server>__<tool>)
// followed by a tool_result for that call that is not an error. A failed call
// proves nothing, since the CLI answers a call to an unconnected server's tool
// with an error result too. Which server a tool belongs to is resolved against
// the registered names afterwards (claudeMCPServersUsed).
type claudeMCPToolUseTracker struct {
	calls map[string]string
	used  map[string]bool
}

func newClaudeMCPToolUseTracker() *claudeMCPToolUseTracker {
	return &claudeMCPToolUseTracker{calls: make(map[string]string)}
}

func (t *claudeMCPToolUseTracker) observe(events []transcriptEvent) {
	for _, event := range events {
		call := event.ToolCall
		if call == nil || call.ID == "" {
			continue
		}
		if event.Role == "assistant" {
			if strings.HasPrefix(call.Name, claudeMCPToolPrefix) {
				t.calls[call.ID] = call.Name
			}
			continue
		}
		tool, ok := t.calls[call.ID]
		if event.Role != "tool" || !ok || call.Success == nil || !*call.Success {
			continue
		}
		if t.used == nil {
			t.used = make(map[string]bool)
		}
		t.used[tool] = true
	}
}

const claudeMCPToolPrefix = "mcp__"

// claudeMCPServersUsed maps each successfully called MCP tool to the
// registered server it belongs to. The claude CLI names a server's tools
// mcp__<normalized server>__<tool>, where the normalized name replaces every
// character outside [A-Za-z0-9_-] with '_'. A server name may itself contain
// "__", so a tool is attributed to the longest registered name whose prefix
// it carries.
func claudeMCPServersUsed(tools map[string]bool, registered []string) map[string]bool {
	used := make(map[string]bool)
	for tool := range tools {
		best := ""
		for _, name := range registered {
			prefix := claudeMCPToolPrefix + claudeMCPNormalizeServerName(name) + "__"
			if len(tool) > len(prefix) && strings.HasPrefix(tool, prefix) && len(name) > len(best) {
				best = name
			}
		}
		if best != "" {
			used[best] = true
		}
	}
	return used
}

// claudeMCPNormalizeServerName mirrors the claude CLI's normalization of a
// server name inside its tool names.
func claudeMCPNormalizeServerName(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, name)
}
