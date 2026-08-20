package harness

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/goobers/goobers/internal/mcpio"
)

// withAutoGoobersIOClaude marks req eligible for the goobers-io MCP server
// when a self-binary path is known to launch it, without touching req.Tools.
// This differs from Copilot's withAutoGoobersIO: confirmed live against the
// installed claude CLI (2.1.227) that --tools/--allowedTools do not gate
// MCP-server-provided tools at all — once a server is registered via
// --mcp-config, every tool it reports is reachable regardless of the
// built-in tool allowlist's content, or even its absence. Threading
// mcp__goobers-io__* names into --tools/--allowedTools (Copilot's approach)
// would be a no-op here, and worse, would flip every eligible run into
// claude-code's tool-constrained --tools path (see claudeExtraArgs) even for
// goobers that declare no Spec.Tools — that's not this issue's concern to
// change. Sets GoobersIORegistered so the shared prompt renderer only
// mentions goobers-io tools once this adapter has actually registered them
// (#2774).
func withAutoGoobersIOClaude(req RunRequest, selfBin string) RunRequest {
	if selfBin == "" || !autoGoobersIOEligible(req) {
		return req
	}
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
// tool sub-allowlist, and none is needed — the live check above confirmed
// registering the server exposes all of goobers-io's tools already.
func goobersIOClaudeMCPConfigArg(req RunRequest, selfBin string) (string, error) {
	if selfBin == "" || !autoGoobersIOEligible(req) {
		return "", nil
	}
	artifactFile, _ := req.Envelope.Inputs[InputArtifactFile].(string)
	cfg := mcpio.Config{
		Workspace:    req.Workspace,
		ArtifactFile: artifactFile,
		ReceiptFile:  goobersIOReceiptFile(),
		Inputs:       req.ContextPaths,
		RunID:        req.Envelope.RunID,
		WorkflowID:   req.Envelope.WorkflowID,
		TaskID:       req.Envelope.TaskID,
		Gaggle:       req.Envelope.Gaggle,
	}
	configRel := filepath.Join(filepath.FromSlash(goobersIORuntimeSubdir), mcpio.ConfigFileName)
	configPath, err := mcpio.WriteConfig(req.Workspace, configRel, cfg)
	if err != nil {
		return "", fmt.Errorf("write goobers-io config: %w", err)
	}
	if err := mcpio.ResetInputInspectionReceipts(req.Workspace, cfg.ReceiptFile); err != nil {
		return "", fmt.Errorf("reset goobers-io input inspection receipts: %w", err)
	}

	server := struct {
		Type    string   `json:"type"`
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}{
		Type:    "stdio",
		Command: selfBin,
		Args:    []string{"mcp-io", "--config", configPath},
	}
	data, err := json.Marshal(map[string]interface{}{
		"mcpServers": map[string]interface{}{goobersIOServerName: server},
	})
	if err != nil {
		return "", fmt.Errorf("encode goobers-io MCP registration: %w", err)
	}
	return string(data), nil
}
