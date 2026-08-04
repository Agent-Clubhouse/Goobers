package harness

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/mcpio"
)

// goobersIOServerName is the fixed name Copilot sees for the goobers-io
// server in a live run's --available-tools (goobers-io-publish_output etc).
const goobersIOServerName = "goobers-io"

// goobersIORuntimeSubdir is the workspace-relative directory the harness
// writes goobers-io's own runtime config into — deliberately not
// COPILOT_HOME-relative, and deliberately never touching req.MCPServers or
// internal/mcpconfig's validation (#2406 review): those exist to protect
// against a genuinely external, possibly-untrusted MCP server, and both
// their COPILOT_HOME-redirection (breaks documented stored-CLI-login auth)
// and their local-server credential-isolation requirement (rejects a
// credential-less local server declared alongside a credentialed one) are
// correct behavior for that case — just aimed at the wrong target when
// applied to a server the harness itself constructs, with no credentials of
// its own, delivered via --additional-mcp-config instead of the shared
// mcp-config.json those checks inspect.
const goobersIORuntimeSubdir = ".goobers/mcp-io"

// goobersIOTools are goobers-io's own tool names, as the server itself
// reports them in its tools/list response — used for the per-server "tools"
// field inside the --additional-mcp-config registration. They carry no
// privileged capability or credential access, so auto-granting them needs no
// per-goober tools: declaration — unlike shell/github, there is no SEC-030
// reason to gate them behind explicit opt-in.
var goobersIOTools = []string{"publish_output", "list_inputs", "read_input", "grep_input"}

// goobersIOAvailableToolNames returns goobersIOTools prefixed the way
// Copilot's own --available-tools allowlist expects an external server's
// tools to be named (<serverName>-<toolName>) — the same convention
// copilotToolGroups["github"] already uses for the built-in GitHub server
// (github-mcp-server-issue_write, etc.). Confirmed live: the bare names
// alone do not resolve against a restrictive --available-tools= for an
// externally-registered server — Copilot silently drops them, leaving zero
// goobers-io tools reachable, even though the same bare names are exactly
// right for the per-server "tools" field in the MCP registration itself.
// These are two different fields with two different naming conventions;
// conflating them is what broke this the first time.
func goobersIOAvailableToolNames() []string {
	out := make([]string, len(goobersIOTools))
	for i, name := range goobersIOTools {
		out[i] = goobersIOServerName + "-" + name
	}
	return out
}

// autoGoobersIOEligible reports whether this invocation has anything for
// goobers-io to handle: a declared artifactFile output, or any
// already-materialized upstream input to read. A task with neither gets no
// auto-wiring — nothing changes for it. This is now the single source of
// truth for whether goobers-io is active this invocation — see
// goobersIOAdditionalMCPConfigArg.
func autoGoobersIOEligible(req RunRequest) bool {
	artifactFile, _ := req.Envelope.Inputs[InputArtifactFile].(string)
	return artifactFile != "" || len(req.ContextPaths) > 0
}

// withAutoGoobersIO grants req.Tools the goobers-io tool names when eligible
// (autoGoobersIOEligible) and a self-binary path is known to launch it. It
// no longer touches req.MCPServers at all — see goobersIORuntimeSubdir's
// doc comment for why. req is passed and returned by value (RunRequest has
// no pointer receiver callers rely on), so this never mutates a caller's
// copy.
func withAutoGoobersIO(req RunRequest, selfBin string) RunRequest {
	if selfBin == "" || !autoGoobersIOEligible(req) {
		return req
	}
	req.Tools = appendMissing(req.Tools, goobersIOAvailableToolNames()...)
	return req
}

// goobersIOAdditionalMCPConfigArg builds the --additional-mcp-config
// argument that registers goobers-io for this invocation, and writes its
// runtime config (workspace, declared artifactFile, materialized upstream
// inputs) to a workspace-relative path passed to the spawned process via
// --config, rather than through $COPILOT_HOME (goobers-io needs no
// COPILOT_HOME redirection — it has no ambient credential to protect
// against leaking, and redirecting it would be what breaks stored-login
// auth for every other eligible stage). Returns ("", nil) when this
// invocation isn't eligible or selfBin is unknown — the caller appends
// nothing in that case.
//
// This write happens in the harness's own process, before the spawned
// copilot subprocess is sandboxed — req.Workspace is the task's own
// worktree, which may contain repository-controlled content. mcpio.WriteConfig
// (not a plain os.MkdirAll+os.WriteFile) is load-bearing here: a symlink
// planted at goobersIORuntimeSubdir, or at any not-yet-existing intermediate
// component of it, must not be followed by this trusted, pre-sandbox write
// (review finding on #2408; see #2413 for the other harness call sites with
// the same gap, not fixed here since they predate this code).
func goobersIOAdditionalMCPConfigArg(req RunRequest, selfBin string) (string, error) {
	if selfBin == "" || !autoGoobersIOEligible(req) {
		return "", nil
	}
	artifactFile, _ := req.Envelope.Inputs[InputArtifactFile].(string)
	cfg := mcpio.Config{
		Workspace:    req.Workspace,
		ArtifactFile: artifactFile,
		Inputs:       req.ContextPaths,
	}
	configRel := filepath.Join(filepath.FromSlash(goobersIORuntimeSubdir), mcpio.ConfigFileName)
	configPath, err := mcpio.WriteConfig(req.Workspace, configRel, cfg)
	if err != nil {
		return "", fmt.Errorf("write goobers-io config: %w", err)
	}

	server := struct {
		Type    string   `json:"type"`
		Command string   `json:"command"`
		Args    []string `json:"args"`
		Tools   []string `json:"tools"`
	}{
		Type:    "local",
		Command: selfBin,
		Args:    []string{"mcp-io", "--config", configPath},
		Tools:   append([]string(nil), goobersIOTools...),
	}
	data, err := json.Marshal(map[string]interface{}{
		"mcpServers": map[string]interface{}{goobersIOServerName: server},
	})
	if err != nil {
		return "", fmt.Errorf("encode goobers-io MCP registration: %w", err)
	}
	return string(data), nil
}

// goobersIOPromptSection explains how to use whichever goobers-io tools this
// invocation actually has a reason to call — only the write directive when
// there's a declared artifactFile, only the read directive when there's
// upstream context, both when there's both. Getting the model to actually
// use these tools instead of a generic file-editing tool requires naming
// them explicitly here — a goober's own instructions.md describing an output
// shape ("artifacts.findingsRef pointing at X.md") is not enough on its own;
// confirmed live (#2406) that with a tool available but not named, the model
// falls back to apply_patch/create every time.
func goobersIOPromptSection(req RunRequest) string {
	artifactFile, _ := req.Envelope.Inputs[InputArtifactFile].(string)
	var b strings.Builder
	b.WriteString("## goobers-io tools\n\n")
	if artifactFile != "" {
		b.WriteString("Call `publish_output` with your complete final output when you are done — do not write it to a file yourself with any other tool.\n\n")
	}
	if len(req.ContextPaths) > 0 {
		b.WriteString("Use `list_inputs`, `grep_input`, and `read_input` to examine the upstream content listed under Context above, instead of opening those files directly. Prefer `grep_input` to search a large input, or `read_input` with a line range around a match, rather than reading a large input in one call.\n\n")
	}
	return b.String()
}

// appendMissing returns existing plus every entry of add not already present
// in it, preserving existing's order and never introducing a duplicate.
func appendMissing(existing []string, add ...string) []string {
	seen := make(map[string]bool, len(existing))
	for _, name := range existing {
		seen[name] = true
	}
	out := append([]string(nil), existing...)
	for _, name := range add {
		if !seen[name] {
			out = append(out, name)
			seen[name] = true
		}
	}
	return out
}
