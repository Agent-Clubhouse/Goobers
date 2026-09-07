package harness

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// copilotMCPImplementationRe extracts the server name from the Copilot CLI's
// own "Service initialized as client" line, which carries the completed MCP
// handshake's ServerPeerInfo as an escaped JSON string:
//
//	[INFO] [rust:rmcp::service] Service initialized as client
//	{"peer_info":"Some(ServerPeerInfo { protocol_version: ...,
//	server_info: Some(Implementation { name: \"goobers-io\", ... }) ... })"}
//
// The name is escaped because it sits inside a JSON string value, so the
// pattern matches the escaped form. Anchoring on `Implementation {` keeps it
// from matching any other `name:` that may appear in the same line.
var copilotMCPImplementationRe = regexp.MustCompile(`Implementation\s*\{\s*name:\s*\\"([^\\"]+)\\"`)

// copilotMCPSubsystemMarkers are substrings that prove the CLI reached its MCP
// machinery at all. Without one of these the log tells us nothing about MCP —
// the process may have died during auth or argument parsing — and absence of
// evidence must not become evidence of absence (mirrors the claude adapter's
// mcpServersReported guard).
var copilotMCPSubsystemMarkers = []string{
	"rmcp::",
	"copilot_runtime::mcp",
	"mcp_policy_check",
}

// copilotMCPStderrPrefix marks the line where the CLI forwards one MCP
// server's stderr. It carries the server name in real (unescaped) JSON, so a
// server that produced output is known to have been launched even if it never
// completed the handshake.
const copilotMCPStderrPrefix = "MCP server stderr "

// Status values reported for a Copilot MCP server that was registered but not
// observed connected.
const (
	// copilotMCPStatusAbsent means nothing in the CLI's log mentions the
	// server at all: it was registered, but no process output and no
	// handshake were ever observed. Typically the server binary failed to
	// launch.
	copilotMCPStatusAbsent = "absent"
	// copilotMCPStatusHandshakeIncomplete means the server process ran and
	// wrote output, but the CLI never reported a completed MCP handshake for
	// it — a protocol-level rejection rather than a launch failure.
	copilotMCPStatusHandshakeIncomplete = "started-no-handshake"
	// copilotMCPStatusPolicyRejected means the CLI refused to load the server
	// at all under its third-party MCP policy (#2955). It is deliberately
	// distinct from the two statuses above: those are faults to investigate,
	// while this is an administrative setting, and the operator actions have
	// nothing in common.
	copilotMCPStatusPolicyRejected = "policy-rejected"
)

// copilotMCPPolicyRejectionMarkers are the CLI's own phrases for declining an
// external MCP server on policy grounds. Observed verbatim:
//
//	Skipping third-party MCP server "goobers-io" because the MCP third-party
//	policy is not enabled
//
// Matched line-scoped and only alongside the server's name, so a policy
// discussion elsewhere in the log cannot be read as a rejection of this
// server.
var copilotMCPPolicyRejectionMarkers = []string{
	"third-party policy is not enabled",
	"third-party mcp policy is not enabled",
	"mcp third-party policy is not enabled",
	"skipping third-party mcp server",
}

// copilotMCPServerFailures compares the MCP servers this invocation registered
// (goobers-io when GoobersIORegistered, plus every declared req.MCPServers
// entry) against the servers the Copilot CLI's own run log shows completing an
// MCP handshake, and returns the ones that were not usable (#3356/#3456).
//
// The claude adapter can do this from a structured system/init event; Copilot
// emits no equivalent in its session transcript, which is why #3428's detection
// was inert on the adapter that actually runs in production. The CLI does,
// however, log its MCP client lifecycle, and this reads that.
//
// Returns nil when the log shows no MCP activity at all — an unreadable log
// directory, or a CLI that exited before reaching its MCP machinery. As on the
// claude side, absence of a report is never treated as proof of absence of the
// servers, so a nil return never asserts the servers WERE available.
func copilotMCPServerFailures(req RunRequest, logDir string) []MCPServerFailure {
	registered := copilotRegisteredMCPServers(req)
	if len(registered) == 0 || logDir == "" {
		return nil
	}
	scan := scanCopilotMCPLog(logDir)
	if !scan.reported {
		return nil
	}
	var failures []MCPServerFailure
	for _, name := range registered {
		if _, ok := scan.connected[name]; ok {
			continue
		}
		status := copilotMCPStatusAbsent
		switch {
		case hasKey(scan.policyRejected, name):
			// Checked first: a server policy refused was never launched, so
			// the absence below would describe it correctly but uselessly.
			status = copilotMCPStatusPolicyRejected
		case hasKey(scan.launched, name):
			status = copilotMCPStatusHandshakeIncomplete
		}
		failures = append(failures, MCPServerFailure{Server: name, Status: status})
	}
	return failures
}

func hasKey(set map[string]struct{}, name string) bool {
	_, ok := set[name]
	return ok
}

// copilotMCPScan is what one invocation's CLI logs said about its MCP servers.
type copilotMCPScan struct {
	connected      map[string]struct{}
	launched       map[string]struct{}
	policyRejected map[string]struct{}
	reported       bool
}

// copilotRegisteredMCPServers returns the deduplicated names this invocation
// registered, in a stable order.
func copilotRegisteredMCPServers(req RunRequest) []string {
	var registered []string
	seen := make(map[string]struct{})
	add := func(name string) {
		if name == "" {
			return
		}
		if _, dup := seen[name]; dup {
			return
		}
		seen[name] = struct{}{}
		registered = append(registered, name)
	}
	if req.GoobersIORegistered {
		add(goobersIOServerName)
	}
	for _, server := range req.MCPServers {
		add(server.Name)
	}
	return registered
}

// scanCopilotMCPLog reads every log file the CLI wrote for this invocation and
// reports which servers completed a handshake, which merely produced output,
// and whether the log contains any MCP activity at all.
func scanCopilotMCPLog(logDir string) copilotMCPScan {
	scan := copilotMCPScan{
		connected:      make(map[string]struct{}),
		launched:       make(map[string]struct{}),
		policyRejected: make(map[string]struct{}),
	}
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return scan
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		names = append(names, entry.Name())
	}
	// Deterministic order so a truncated read is reproducible.
	sort.Strings(names)
	for _, name := range names {
		if scanCopilotMCPLogFile(filepath.Join(logDir, name), &scan) {
			scan.reported = true
		}
	}
	return scan
}

// scanCopilotMCPLogFile scans one log file, recording handshake completions and
// launched-but-silent servers. Reports whether this file showed MCP activity.
func scanCopilotMCPLogFile(path string, scan *copilotMCPScan) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	// Copilot writes the whole ServerPeerInfo — including base64 icon data —
	// on a single line, so the default 64KiB limit is far too small.
	scanner.Buffer(make([]byte, 0, 64*1024), maxCopilotMCPLogLineBytes)
	sawMCP := false
	for scanner.Scan() {
		line := scanner.Text()
		if !sawMCP {
			for _, marker := range copilotMCPSubsystemMarkers {
				if strings.Contains(line, marker) {
					sawMCP = true
					break
				}
			}
		}
		if match := copilotMCPImplementationRe.FindStringSubmatch(line); match != nil {
			scan.connected[match[1]] = struct{}{}
			scan.launched[match[1]] = struct{}{}
			continue
		}
		if name, ok := copilotMCPPolicyRejectedServerName(line); ok {
			scan.policyRejected[name] = struct{}{}
			sawMCP = true
			continue
		}
		if name, ok := copilotMCPStderrServerName(line); ok {
			scan.launched[name] = struct{}{}
		}
	}
	// A line longer than the buffer stops the scan; treat whatever was seen
	// before it as valid rather than discarding the file.
	return sawMCP
}

// maxCopilotMCPLogLineBytes bounds a single log line. The handshake line
// embeds base64 icons from remote servers, so it is routinely tens of KiB.
const maxCopilotMCPLogLineBytes = 4 << 20

// copilotMCPStderrServerName extracts the server name from a forwarded-stderr
// line. Unlike the handshake line, its payload is ordinary JSON.
func copilotMCPStderrServerName(line string) (string, bool) {
	idx := strings.Index(line, copilotMCPStderrPrefix)
	if idx < 0 {
		return "", false
	}
	payload := strings.TrimSpace(line[idx+len(copilotMCPStderrPrefix):])
	if !strings.HasPrefix(payload, "{") {
		return "", false
	}
	var record struct {
		ServerName string `json:"server_name"`
	}
	if err := json.Unmarshal([]byte(payload), &record); err != nil {
		return "", false
	}
	if record.ServerName == "" {
		return "", false
	}
	return record.ServerName, true
}

// copilotMCPPolicyRejectedServerName extracts the server name from a
// policy-rejection line, if the line is one (#2955).
//
// The CLI names the server in quotes: Skipping third-party MCP server
// "goobers-io" because the MCP third-party policy is not enabled. The quoted
// name is required, so a policy message that names no server — or a line where
// the phrase appears for another reason — yields nothing rather than a guess.
func copilotMCPPolicyRejectedServerName(line string) (string, bool) {
	lower := strings.ToLower(line)
	matched := false
	for _, marker := range copilotMCPPolicyRejectionMarkers {
		if strings.Contains(lower, marker) {
			matched = true
			break
		}
	}
	if !matched {
		return "", false
	}
	open := strings.Index(line, `"`)
	if open < 0 {
		return "", false
	}
	rest := line[open+1:]
	close := strings.Index(rest, `"`)
	if close <= 0 {
		return "", false
	}
	return rest[:close], true
}
