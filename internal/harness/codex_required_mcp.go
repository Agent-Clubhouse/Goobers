package harness

import (
	"bytes"
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

// codexRequiredMCPStartupError classifies a failed Codex invocation whose CLI
// refused to start because the auto-wired goobers-io server failed to
// initialize (#5397). Such a failure never reached the model, so it is the
// same infrastructure fault the Copilot pre-model probe reports: it wraps
// errRequiredMCPUnavailable, which the executor marks as an infrastructure
// failure carrying HARNESS_REQUIRED_MCP_UNAVAILABLE, and it records a
// transport_failure readiness observation.
//
// Only goobers-io is classified. A declared server is also registered as
// required, but its startup failure is usually a configuration error that a
// retry will not fix, so it keeps the ordinary harness error. The CLI's own
// server error text is not copied into the error or the annotation.
func codexRequiredMCPStartupError(req RunRequest, result ProcessResult, err error) error {
	if err == nil || !req.GoobersIORegistered {
		return err
	}
	if !codexRequiredMCPStartupFailed(result.Stderr, goobersIOServerName) &&
		!codexRequiredMCPStartupFailed(result.Transcript, goobersIOServerName) {
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
