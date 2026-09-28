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
type MCPReadiness struct {
	Server        string `json:"server"`
	Category      string `json:"category"`
	Source        string `json:"source"`
	Connection    string `json:"connection"`
	Inventory     string `json:"inventory"`
	Authorization string `json:"authorization"`
}

const requiredMCPProbeTimeout = 15 * time.Second

// requiredMCPSession is implemented by the same session that receives the
// model prompt. A separate process's successful handshake is not sufficient.
type requiredMCPSession interface {
	InitializeTools(context.Context) error
	ListMCP(context.Context) (*rpc.MCPServerList, error)
	ListMCPTools(context.Context, string) (*rpc.MCPListToolsResult, error)
	ExecuteTool(context.Context, string) (rpc.ToolResult, error)
}

func probeRequiredMCPSession(ctx context.Context, session requiredMCPSession) (MCPReadiness, error) {
	ctx, cancel := context.WithTimeout(ctx, requiredMCPProbeTimeout)
	defer cancel()
	report := MCPReadiness{Server: goobersIOServerName, Source: "adapter-session", Connection: "unobservable", Inventory: "unobservable", Authorization: "unobservable"}
	if err := session.InitializeTools(ctx); err != nil {
		return mcpProbeFailure(report, "transport_failure", errRequiredMCPUnavailable)
	}
	servers, err := awaitRequiredMCPSettled(ctx, session)
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
		return probeRequiredMCPTools(ctx, session, report)
	}
	return mcpProbeFailure(report, "required_tool_unavailable", errRequiredMCPUnavailable)
}

// requiredMCPSettlePoll paces re-listing while the session is still starting
// its MCP servers.
const requiredMCPSettlePoll = 250 * time.Millisecond

// awaitRequiredMCPSettled lists the session's MCP servers until goobers-io has
// left its startup state or the probe window closes (#5397).
//
// The CLI starts session MCP servers asynchronously: a list taken straight
// after session creation can report the server "pending", or report no
// servers at all before the MCP host has initialized. Treating that first
// snapshot as final failed stages whose server came up moments later, and
// under load that was most of the observed "required_tool_unavailable"
// failures. Only those two startup states are waited on; every settled status,
// including absence from an initialized host, is returned for the caller to
// judge exactly as before, and a window that closes while still starting
// returns the last snapshot, which the caller reports as unavailable.
func awaitRequiredMCPSettled(ctx context.Context, session requiredMCPSession) (*rpc.MCPServerList, error) {
	for {
		servers, err := session.ListMCP(ctx)
		if err != nil || !requiredMCPStarting(servers) {
			return servers, err
		}
		timer := time.NewTimer(requiredMCPSettlePoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return servers, nil
		case <-timer.C:
		}
	}
}

func requiredMCPStarting(servers *rpc.MCPServerList) bool {
	if servers == nil {
		return false
	}
	for _, server := range servers.Servers {
		if server.Name == goobersIOServerName {
			return server.Status == rpc.MCPServerStatusPending
		}
	}
	return servers.Host == nil
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
