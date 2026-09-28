package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// codexMCPReadinessSource is the required-mcp-readiness source the codex
// adapter reports (#5397). Goobers cannot observe goobers-io before the model
// turn, but it registers the server with `required = true`, so the Codex CLI
// itself refuses to start a session without it: readiness is enforced at CLI
// startup rather than probed by the adapter.
const codexMCPReadinessSource = "startup-required"

// codexRequiredMCPStartupMarker is the text the Codex CLI writes to stderr,
// before exiting non-zero and before any model turn, when a server registered
// with `required = true` fails to initialize. Observed on codex-cli 0.157.0:
//
//	... Failed to initialize session: required MCP servers failed to initialize: goobers-io: handshaking with MCP server failed: ...
//
// Several failed servers are joined with "; ", each as "<name>: <error>".
const codexRequiredMCPStartupMarker = "required MCP servers failed to initialize: "

// codexInitialCheckpoint is the transcript checkpoint of a codex stage's
// first invocation. Only that invocation's startup is observed for required
// MCP readiness: a completion-repair resume starts after the model already
// ran, so its startup failure is not a before-model fault and keeps the
// ordinary harness error.
const codexInitialCheckpoint = 1

// observeCodexRequiredMCPStartup records what a codex stage's first
// invocation shows about the auto-wired goobers-io server (#5397), and
// classifies a startup refusal.
//
// A session that started at all (a thread.started or turn.started event) is
// evidence goobers-io initialized, since the CLI refuses to start without a
// `required = true` server. That records a check_unobservable observation
// with connection and inventory ready, which clears an earlier availability
// failure for the same context; authorization stays unobservable. A started
// session is never reclassified, whatever its later failure says.
//
// A failed invocation that never started a session and whose stderr carries
// the CLI's required-server refusal naming goobers-io never reached the model:
// it is the same infrastructure fault the Copilot pre-model probe reports. It
// wraps errRequiredMCPUnavailable, which the executor marks as an
// infrastructure failure carrying HARNESS_REQUIRED_MCP_UNAVAILABLE, and
// records a transport_failure observation. Only stderr is read: stdout
// carries the model's tool output, which may quote the marker text.
//
// Only goobers-io is classified. A declared server is also registered as
// required, but its startup failure is usually a configuration error that a
// retry will not fix, so it keeps the ordinary harness error. The CLI's own
// server error text is not copied into the error or the annotation.
func observeCodexRequiredMCPStartup(req RunRequest, result ProcessResult, sessionStarted bool, err error) error {
	if !req.GoobersIORegistered {
		return err
	}
	if sessionStarted {
		return readinessReportedError(req, MCPReadiness{
			Server:        goobersIOServerName,
			Category:      "check_unobservable",
			Source:        codexMCPReadinessSource,
			Connection:    "ready",
			Inventory:     "ready",
			Authorization: "unobservable",
		}, err)
	}
	if err == nil || !codexRequiredMCPStartupFailed(result.Stderr, goobersIOServerName) {
		return err
	}
	classified := errors.Join(fmt.Errorf(
		"%w: codex refused to start the session because the required %s MCP server failed to initialize",
		errRequiredMCPUnavailable, goobersIOServerName), err)
	return readinessReportedError(req, MCPReadiness{
		Server:        goobersIOServerName,
		Category:      "transport_failure",
		Source:        codexMCPReadinessSource,
		Connection:    "unobservable",
		Inventory:     "unobservable",
		Authorization: "unobservable",
	}, classified)
}

// codexSessionStarted reports whether a codex invocation got past session
// startup. The JSONL stdout capture is authoritative; when it saw no data (a
// runner that does not feed it), the combined transcript's parseable event
// lines are consulted instead.
func codexSessionStarted(stdout *codexJSONLCapture, transcript []byte) bool {
	if stdout.hasData() {
		return stdout.sessionStarted()
	}
	for len(transcript) > 0 {
		var line []byte
		line, transcript, _ = bytes.Cut(transcript, []byte{'\n'})
		line = bytes.TrimSpace(line)
		if !json.Valid(line) {
			continue
		}
		parsed := codexParseResult{metrics: map[string]float64{}}
		if parseCodexEvent(line, &parsed) == nil && parsed.started {
			return true
		}
	}
	return false
}

// codexRequiredMCPStartupFailed reports whether output carries the Codex CLI's
// required-server startup failure naming server.
func codexRequiredMCPStartupFailed(output []byte, server string) bool {
	for len(output) > 0 {
		var line []byte
		line, output, _ = bytes.Cut(output, []byte{'\n'})
		_, failures, found := bytes.Cut(line, []byte(codexRequiredMCPStartupMarker))
		if !found {
			continue
		}
		for _, failure := range strings.Split(string(failures), "; ") {
			if strings.HasPrefix(failure, server+":") {
				return true
			}
		}
	}
	return false
}
