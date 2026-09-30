package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/github/copilot-sdk/go/rpc"
)

// MCPReadiness describes an actual adapter session's pre-model observation.
// It deliberately excludes server errors and tool responses, which may contain
// credentials or workspace content. Unobservable never means ready.
//
// The diagnostic fields say which startup sub-state a failed check ended in
// (#5397). ObservedStatus is one of a closed set: a goobers-io MCPServerStatus
// the adapter recognizes, "unknown" for any other status string,
// "pending-connection", "host-uninitialized", "absent" or "unobserved"; it is
// never a raw server message. FailedReasonPresent records only whether the
// MCP host holds a connection failure for goobers-io, never its text.
type MCPReadiness struct {
	Server              string `json:"server"`
	Category            string `json:"category"`
	Source              string `json:"source"`
	Connection          string `json:"connection"`
	Inventory           string `json:"inventory"`
	Authorization       string `json:"authorization"`
	ObservedStatus      string `json:"observedStatus,omitempty"`
	Polls               int    `json:"polls,omitempty"`
	ElapsedMs           int64  `json:"elapsedMs,omitempty"`
	FailedReasonPresent bool   `json:"failedReasonPresent,omitempty"`
}

// requiredMCPProbeTimeout caps each bounded readiness phase other than the
// startup settle wait: InitializeTools, and the inventory plus native
// authorization checks.
const requiredMCPProbeTimeout = 15 * time.Second

// DefaultRequiredMCPSettleTimeout is how long the probe waits, by default, for
// goobers-io to leave its startup state. It is its own budget, so a slow
// InitializeTools no longer shortens it (#5397). Operators can change it with
// runner.requiredMCPSettleTimeout.
const DefaultRequiredMCPSettleTimeout = 30 * time.Second

// Observed statuses that are not an MCPServerStatus.
const (
	mcpObservedUnobserved        = "unobserved"
	mcpObservedHostUninitialized = "host-uninitialized"
	mcpObservedPendingConnection = "pending-connection"
	mcpObservedAbsent            = "absent"
	mcpObservedUnknown           = "unknown"
)

// requiredMCPSettleBudget resolves a configured settle budget; zero or a
// negative value keeps the default.
func requiredMCPSettleBudget(configured time.Duration) time.Duration {
	if configured <= 0 {
		return DefaultRequiredMCPSettleTimeout
	}
	return configured
}

// requiredMCPSession is implemented by the same session that receives the
// model prompt. A separate process's successful handshake is not sufficient.
type requiredMCPSession interface {
	InitializeTools(context.Context) error
	ListMCP(context.Context) (*rpc.MCPServerList, error)
	ListMCPTools(context.Context, string) (*rpc.MCPListToolsResult, error)
	ExecuteTool(context.Context, string) (rpc.ToolResult, error)
}

// probeRequiredMCPSession checks the session that will receive the model
// prompt. settle bounds only the wait for goobers-io to leave startup; zero
// selects DefaultRequiredMCPSettleTimeout. The report carries the elapsed
// time from probe start whatever the outcome.
func probeRequiredMCPSession(ctx context.Context, session requiredMCPSession, settle time.Duration) (MCPReadiness, error) {
	started := time.Now()
	report := MCPReadiness{Server: goobersIOServerName, Source: "adapter-session", Connection: "unobservable", Inventory: "unobservable", Authorization: "unobservable", ObservedStatus: mcpObservedUnobserved}
	report, err := probeRequiredMCPPhases(ctx, session, requiredMCPSettleBudget(settle), report)
	report.ElapsedMs = time.Since(started).Milliseconds()
	return report, err
}

func probeRequiredMCPPhases(ctx context.Context, session requiredMCPSession, settle time.Duration, report MCPReadiness) (MCPReadiness, error) {
	initCtx, cancelInit := context.WithTimeout(ctx, requiredMCPProbeTimeout)
	err := session.InitializeTools(initCtx)
	cancelInit()
	if err != nil {
		return mcpProbeFailure(report, "transport_failure", errRequiredMCPUnavailable)
	}
	servers, polls, err := awaitRequiredMCPSettled(ctx, session, settle)
	report.Polls = polls
	report.ObservedStatus, report.FailedReasonPresent = observeRequiredMCP(servers)
	if err != nil {
		return mcpProbeFailure(report, "transport_failure", errRequiredMCPUnavailable)
	}
	if servers == nil {
		return mcpProbeFailure(report, "required_tool_unavailable", errRequiredMCPUnavailable)
	}
	if servers.Host != nil && slices.Contains(servers.Host.FilteredServers, goobersIOServerName) {
		return mcpProbeFailure(report, "tool_authorization_failure", errRequiredMCPRejected)
	}
	for _, server := range servers.Servers {
		if server.Name != goobersIOServerName {
			continue
		}
		if server.Status == "needs-auth" {
			return mcpProbeFailure(report, "authentication_failure", errRequiredMCPRejected)
		}
		if server.Status == "disabled" {
			return mcpProbeFailure(report, "tool_authorization_failure", errRequiredMCPRejected)
		}
		if server.Status != "connected" {
			return mcpProbeFailure(report, "required_tool_unavailable", errRequiredMCPUnavailable)
		}
		report.Connection = "ready"
		toolsCtx, cancelTools := context.WithTimeout(ctx, requiredMCPProbeTimeout)
		defer cancelTools()
		return probeRequiredMCPTools(toolsCtx, session, report)
	}
	return mcpProbeFailure(report, "required_tool_unavailable", errRequiredMCPUnavailable)
}

// observeRequiredMCP reduces the last server list to credential-free
// diagnostics: a closed-set status and whether a failure was recorded.
func observeRequiredMCP(servers *rpc.MCPServerList) (string, bool) {
	if servers == nil {
		return mcpObservedUnobserved, false
	}
	failed := false
	pending := false
	if servers.Host != nil {
		_, failed = servers.Host.FailedServers[goobersIOServerName]
		pending = slices.Contains(servers.Host.PendingConnections, goobersIOServerName)
	}
	for _, server := range servers.Servers {
		if server.Name == goobersIOServerName {
			return knownMCPServerStatus(server.Status), failed
		}
	}
	switch {
	case servers.Host == nil:
		return mcpObservedHostUninitialized, failed
	case pending:
		return mcpObservedPendingConnection, failed
	default:
		return mcpObservedAbsent, failed
	}
}

// knownMCPServerStatus keeps the annotation a closed set even if a runtime
// reports a status string this build does not know.
func knownMCPServerStatus(status rpc.MCPServerStatus) string {
	switch status {
	case rpc.MCPServerStatusConnected, rpc.MCPServerStatusDisabled, rpc.MCPServerStatusFailed, rpc.MCPServerStatusNeedsAuth,
		rpc.MCPServerStatusNotConfigured, rpc.MCPServerStatusPending, rpc.MCPServerStatusStopped:
		return string(status)
	default:
		return mcpObservedUnknown
	}
}

// requiredMCPSettlePoll paces re-listing while the session is still starting
// its MCP servers.
const requiredMCPSettlePoll = 250 * time.Millisecond

// awaitRequiredMCPSettled lists the session's MCP servers until goobers-io has
// left its startup state or the settle budget closes (#5397).
//
// The CLI starts session MCP servers asynchronously: a list taken straight
// after session creation can report the server "pending", list it only among
// the host's in-flight connections, or report no servers at all before the
// MCP host has initialized. Treating that first snapshot as final failed
// stages whose server came up moments later, and under load that was most of
// the observed "required_tool_unavailable" failures. Only those startup states
// are waited on; every settled status, including absence from an initialized
// host, is returned for the caller to judge exactly as before, and a budget
// that closes while still starting returns the last snapshot, which the caller
// reports as unavailable. The budget is separate from InitializeTools, so a
// slow initialization does not shorten it. It returns the number of lists
// taken, and on a list error the last good snapshot for diagnostics.
func awaitRequiredMCPSettled(ctx context.Context, session requiredMCPSession, settle time.Duration) (*rpc.MCPServerList, int, error) {
	ctx, cancel := context.WithTimeout(ctx, settle)
	defer cancel()
	var last *rpc.MCPServerList
	for polls := 1; ; polls++ {
		servers, err := session.ListMCP(ctx)
		if err != nil {
			return last, polls, err
		}
		if !requiredMCPStarting(servers) {
			return servers, polls, nil
		}
		last = servers
		timer := time.NewTimer(requiredMCPSettlePoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return servers, polls, nil
		case <-timer.C:
		}
	}
}

// requiredMCPStarting reports whether goobers-io is still starting: listed as
// pending, listed in the host's in-flight connections without having
// connected, or not yet visible because the MCP host has not initialized.
func requiredMCPStarting(servers *rpc.MCPServerList) bool {
	if servers == nil {
		return false
	}
	connecting := servers.Host != nil && slices.Contains(servers.Host.PendingConnections, goobersIOServerName)
	for _, server := range servers.Servers {
		if server.Name == goobersIOServerName {
			return server.Status == rpc.MCPServerStatusPending || connecting && server.Status != rpc.MCPServerStatusConnected
		}
	}
	return servers.Host == nil || connecting
}

func probeRequiredMCPTools(ctx context.Context, session requiredMCPSession, report MCPReadiness) (MCPReadiness, error) {
	tools, err := session.ListMCPTools(ctx, goobersIOServerName)
	if err != nil {
		return mcpProbeFailure(report, "transport_failure", errRequiredMCPUnavailable)
	}
	if tools == nil {
		return mcpProbeFailure(report, "required_tool_unavailable", errRequiredMCPUnavailable)
	}
	for _, required := range goobersIOTools {
		if !slices.ContainsFunc(tools.Tools, func(tool rpc.MCPTools) bool { return tool.Name == required }) {
			return mcpProbeFailure(report, "required_tool_unavailable", errRequiredMCPUnavailable)
		}
	}
	report.Inventory = "ready"
	// get_run_info reads existing invocation identity and does not create input
	// inspection receipts or publish artifacts. Execute uses the native session
	// authorization pipeline rather than a direct connection to the server.
	result, err := session.ExecuteTool(ctx, goobersIOServerName+"-get_run_info")
	if nativeMCPProbeUnsupported(err) {
		report.Category = "check_unobservable"
		return report, nil
	}
	if err != nil {
		return mcpProbeFailure(report, "transport_failure", errRequiredMCPUnavailable)
	}
	if expanded := expandedMCPToolResult(result); expanded != nil {
		switch expanded.ResultType {
		case rpc.ToolResultTypeDenied, rpc.ToolResultTypeRejected:
			return mcpProbeFailure(report, "tool_authorization_failure", errRequiredMCPRejected)
		case rpc.ToolResultTypeSuccess:
		default:
			return mcpProbeFailure(report, "required_tool_unavailable", errRequiredMCPUnavailable)
		}
	}
	if result == nil {
		return mcpProbeFailure(report, "required_tool_unavailable", errRequiredMCPUnavailable)
	}
	report.Authorization = "ready"
	report.Category = "ready"
	return report, nil
}

var errRequiredMCPRejected = errors.New("required MCP tool authorization rejected")

func mcpProbeFailure(report MCPReadiness, category string, cause error) (MCPReadiness, error) {
	report.Category = category
	if category == "authentication_failure" || category == "tool_authorization_failure" {
		report.Authorization = "denied"
	}
	return report, fmt.Errorf("%w: %s (%s)", cause, report.Server, category)
}

// Older CLI runtimes expose connection and inventory RPCs but lack the native
// authorization probe. Only a structured JSON-RPC method-not-found response
// permits this partial observation; transport errors must still stop dispatch.
func nativeMCPProbeUnsupported(err error) bool {
	if err == nil {
		return false
	}
	var response struct {
		Code int `json:"code"`
	}
	encoded, marshalErr := json.Marshal(err)
	return marshalErr == nil && json.Unmarshal(encoded, &response) == nil && response.Code == -32601
}

func expandedMCPToolResult(result rpc.ToolResult) *rpc.ToolResultExpanded {
	switch value := result.(type) {
	case *rpc.ToolResultExpanded:
		return value
	case rpc.ToolResultExpanded:
		return &value
	default:
		return nil
	}
}
