package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/mcpio"
)

// mcpIOTestLine renders one newline-delimited JSON-RPC request in the framing
// the MCP stdio transport uses.
func mcpIOTestLine(t *testing.T, id int, method string, params interface{}) string {
	t.Helper()
	req := map[string]interface{}{"jsonrpc": "2.0", "method": method}
	if id >= 0 {
		req["id"] = id
	}
	if params != nil {
		req["params"] = params
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return string(data) + "\n"
}

// mcpIOTestStdin points os.Stdin at a file holding session, restoring the real
// one when the test ends. runMCPIO reads os.Stdin directly — that is the whole
// point of exercising it rather than mcpio.Server.Serve, since the process the
// harness spawns is this one. A file (rather than a pipe) means the session is
// fully buffered up front and the server sees a clean EOF, so there is nothing
// to deadlock on.
func mcpIOTestStdin(t *testing.T, session string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(session), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path) //nolint:gosec // test-controlled path under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdin
	os.Stdin = file
	t.Cleanup(func() {
		os.Stdin = previous
		_ = file.Close()
	})
}

// mcpIOTestConfig seeds a workspace with one upstream input and writes the
// runtime config the harness would have written before invocation, returning
// the config path to pass to --config.
func mcpIOTestConfig(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	contextDir := filepath.Join(ws, ".goobers", "context")
	if err := os.MkdirAll(contextDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "00-notes_0_"), []byte("upstream content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath, err := mcpio.WriteConfig(ws, filepath.Join(".goobers", "mcp-io", mcpio.ConfigFileName), mcpio.Config{
		Workspace:    ws,
		ArtifactFile: "findings.md",
		ReceiptFile:  filepath.Join(".goobers", "mcp-io", mcpio.ReceiptFileName),
		Inputs:       map[string]string{"notes.artifact[0]": ".goobers/context/00-notes_0_"},
		RunID:        "run-3457",
		WorkflowID:   "implementation",
		TaskID:       "implement",
		Gaggle:       "goobers",
	})
	if err != nil {
		t.Fatal(err)
	}
	return configPath
}

// TestRunMCPIOHandshakeWithCopilotCLIProtocolVersion drives the real
// `goobers mcp-io` entry point — the process the harness spawns via
// --additional-mcp-config, reading the config it wrote and speaking over
// os.Stdin/stdout — with the exact initialize request GitHub Copilot CLI
// 1.0.80 sends.
//
// This is the shape of #3457 rather than a unit test of the negotiation
// helper. The server answered protocolVersion "2025-11-25" with a -32602, the
// CLI recorded "failed to initialize MCP client" and dropped the server, and
// the visible consequence was not a protocol complaint anywhere in the run's
// artifacts — it was that every agentic stage came up with no goobers-io
// toolset at all, 50 invocations out of 50 on the live pod. So the assertion
// that matters is both halves: the handshake succeeds, and the tools are
// actually reachable across it.
func TestRunMCPIOHandshakeWithCopilotCLIProtocolVersion(t *testing.T) {
	configPath := mcpIOTestConfig(t)

	var session strings.Builder
	session.WriteString(mcpIOTestLine(t, 1, "initialize", map[string]interface{}{
		"protocolVersion": "2025-11-25",
		"capabilities":    map[string]interface{}{},
		"clientInfo":      map[string]interface{}{"name": "copilot", "version": "1.0.80"},
	}))
	session.WriteString(mcpIOTestLine(t, -1, "notifications/initialized", nil))
	session.WriteString(mcpIOTestLine(t, 2, "tools/list", map[string]interface{}{}))
	session.WriteString(mcpIOTestLine(t, 3, "tools/call", map[string]interface{}{
		"name": "list_inputs",
	}))
	mcpIOTestStdin(t, session.String())

	var stdout, stderr bytes.Buffer
	if code := runMCPIO([]string{"--config", configPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("runMCPIO exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 responses (initialize, tools/list, tools/call), got %d: %v", len(lines), lines)
	}

	var initialize struct {
		Result struct {
			ProtocolVersion string                 `json:"protocolVersion"`
			Capabilities    map[string]interface{} `json:"capabilities"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &initialize); err != nil {
		t.Fatalf("bad initialize response %q: %v", lines[0], err)
	}
	if initialize.Error != nil {
		t.Fatalf("initialize with protocolVersion 2025-11-25 failed the handshake: %+v", initialize.Error)
	}
	if initialize.Result.ProtocolVersion != "2025-11-25" {
		t.Fatalf("negotiated protocolVersion = %q, want %q",
			initialize.Result.ProtocolVersion, "2025-11-25")
	}
	if _, ok := initialize.Result.Capabilities["tools"]; !ok {
		t.Fatalf("initialize result declares no tools capability: %+v", initialize.Result.Capabilities)
	}

	// The toolset is reachable across the handshake — this is what the
	// outage actually cost.
	for _, tool := range []string{"get_run_info", "publish_output", "list_inputs", "read_input", "grep_input"} {
		if !strings.Contains(lines[1], `"`+tool+`"`) {
			t.Fatalf("tools/list is missing %q: %s", tool, lines[1])
		}
	}
	if !strings.Contains(lines[2], "notes.artifact[0]") || strings.Contains(lines[2], `"isError":true`) {
		t.Fatalf("list_inputs over the negotiated session did not return the seeded input: %s", lines[2])
	}

	if want := "requested=2025-11-25 agreed=2025-11-25"; !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q, want it to record %q", stderr.String(), want)
	}
}

// TestRunMCPIOHandshakeWithUnknownProtocolVersion is the durability half of
// #3457: pinning 2025-11-25 into the supported list fixes today's CLI, but the
// next bump past it must not re-open the outage. An unrecognized revision has
// to come back as a successful initialize naming a version this server does
// support, leaving the decision to the client, and it has to say so on our own
// stderr — the original failure was invisible on the Goobers side for seven
// occurrences because it was only ever written to the CLI's private log.
func TestRunMCPIOHandshakeWithUnknownProtocolVersion(t *testing.T) {
	configPath := mcpIOTestConfig(t)

	var session strings.Builder
	session.WriteString(mcpIOTestLine(t, 1, "initialize", map[string]interface{}{
		"protocolVersion": "2027-06-18",
		"capabilities":    map[string]interface{}{},
		"clientInfo":      map[string]interface{}{"name": "some-future-cli", "version": "9"},
	}))
	session.WriteString(mcpIOTestLine(t, 2, "tools/list", map[string]interface{}{}))
	mcpIOTestStdin(t, session.String())

	var stdout, stderr bytes.Buffer
	if code := runMCPIO([]string{"--config", configPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("runMCPIO exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}

	first := strings.SplitN(strings.TrimSpace(stdout.String()), "\n", 2)[0]
	var initialize struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(first), &initialize); err != nil {
		t.Fatalf("bad initialize response %q: %v", first, err)
	}
	if initialize.Error != nil {
		t.Fatalf("an unknown protocolVersion must negotiate, not fail the handshake: %+v", initialize.Error)
	}
	if initialize.Result.ProtocolVersion == "" || initialize.Result.ProtocolVersion == "2027-06-18" {
		t.Fatalf("negotiated protocolVersion = %q, want a version this server actually supports",
			initialize.Result.ProtocolVersion)
	}
	if !strings.Contains(stderr.String(), "requested=2027-06-18 agreed="+initialize.Result.ProtocolVersion) {
		t.Fatalf("stderr = %q, want it to record the downgrade from 2027-06-18", stderr.String())
	}
}
