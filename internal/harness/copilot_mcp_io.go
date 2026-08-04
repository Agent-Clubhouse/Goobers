package harness

import (
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/mcpio"
)

// goobersIOServerName is the reserved MCP server name a goober declares to
// opt into the generic publish_output/list_inputs/read_input tools (#2406).
// It carries a real command/args like any other declared server — see
// docs/guides — this constant only marks when the harness should also write
// mcpio.Config alongside the MCP registration itself.
const goobersIOServerName = "goobers-io"

// declaresGoobersIO reports whether req.MCPServers includes the reserved
// goobers-io server name.
func declaresGoobersIO(req RunRequest) bool {
	for _, server := range req.MCPServers {
		if server.Name == goobersIOServerName {
			return true
		}
	}
	return false
}

// writeGoobersIOConfig writes mcpio.Config into mcpHome (the same scoped
// COPILOT_HOME prepareCopilotMCP just created) so the spawned goobers-io
// server process can find its own workspace, declared artifactFile target,
// and already-materialized upstream inputs purely by reading
// $COPILOT_HOME/goobers-io-config.json — no other coordination with the
// harness needed. req.ContextPaths is exactly the name->path map
// materializeContext already computed for this invocation's prompt; this is
// a second consumer of the same data, not a new resolution step.
func writeGoobersIOConfig(req RunRequest, mcpHome string) error {
	artifactFile, _ := req.Envelope.Inputs[InputArtifactFile].(string)
	cfg := mcpio.Config{
		Workspace:    req.Workspace,
		ArtifactFile: artifactFile,
		Inputs:       req.ContextPaths,
	}
	return mcpio.WriteConfig(filepath.Join(mcpHome, mcpio.ConfigFileName), cfg)
}

// goobersIOTools are the tool names auto-granted whenever goobers-io
// auto-activates. They carry no privileged capability or credential access —
// unlike shell/github, there is no SEC-030 reason to gate them behind an
// explicit per-goober tools: declaration.
var goobersIOTools = []string{"publish_output", "list_inputs", "read_input", "grep_input"}

// autoGoobersIOEligible reports whether this invocation has anything for
// goobers-io to handle: a declared artifactFile output, or any
// already-materialized upstream input to read. A task with neither gets no
// auto-wiring — nothing changes for it.
func autoGoobersIOEligible(req RunRequest) bool {
	artifactFile, _ := req.Envelope.Inputs[InputArtifactFile].(string)
	return artifactFile != "" || len(req.ContextPaths) > 0
}

// withAutoGoobersIO returns req with the goobers-io MCP server and its tools
// added, when this invocation is eligible (autoGoobersIOEligible), a
// self-binary path is known to launch it, and the goober hasn't already
// declared its own goobers-io server (mcpServers: is additive — a goober may
// still declare a genuinely different external server alongside this; this
// only fills in what's missing, never overrides an explicit declaration).
// req is passed and returned by value (RunRequest has no pointer receiver
// callers rely on), so this never mutates a caller's copy.
func withAutoGoobersIO(req RunRequest, selfBin string) RunRequest {
	if selfBin == "" || !autoGoobersIOEligible(req) || declaresGoobersIO(req) {
		return req
	}
	req.MCPServers = append(append([]apiv1.MCPServer(nil), req.MCPServers...), apiv1.MCPServer{
		Name:    goobersIOServerName,
		Command: selfBin,
		Args:    []string{"mcp-io"},
	})
	req.Tools = appendMissing(req.Tools, goobersIOTools...)
	return req
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
