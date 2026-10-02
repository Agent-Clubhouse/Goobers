package harness

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/github/copilot-sdk/go/rpc"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// MCPDiagnostic separates declared identities from observations made by a
// disposable adapter session. It is not an attestation of the target worker.
// Server errors, instructions, credentials and tool response bodies are omitted.
type MCPDiagnostic struct {
	MCPReadiness
	ConfiguredSource    string   `json:"configuredSource"`
	ConfiguredTransport string   `json:"configuredTransport"`
	NegotiatedServer    string   `json:"negotiatedServer,omitempty"`
	IdentitySource      string   `json:"identitySource"`
	RequiredTools       []string `json:"requiredTools"`
	AvailableTools      []string `json:"availableTools,omitempty"`
	MissingTools        []string `json:"missingTools,omitempty"`
	InventorySource     string   `json:"inventorySource"`
	Probe               string   `json:"probe,omitempty"`
	configuredServers   []string
}

// ConfiguredMCPDiagnostics describes the built-in server and declared external
// servers without executing anything. External tools are never classified as
// safe from a name, description, or readOnlyHint.
func ConfiguredMCPDiagnostics(servers []apiv1.MCPServer, tools []string) []MCPDiagnostic {
	result := []MCPDiagnostic{configuredMCPDiagnostic(goobersIOServerName, goobersIOTools)}
	result[0].ConfiguredTransport = "stdio"
	for _, server := range servers {
		row := configuredMCPDiagnostic(server.Name, tools)
		row.ConfiguredTransport = "http"
		if server.Command != "" {
			row.ConfiguredTransport = "stdio"
		}
		result = append(result, row)
	}
	var names []string
	for _, row := range result {
		names = append(names, row.Server)
	}
	for i := range result {
		result[i].configuredServers = names
	}
	return result
}

func configuredMCPDiagnostic(name string, tools []string) MCPDiagnostic {
	return MCPDiagnostic{MCPReadiness: MCPReadiness{Server: name, Category: "check_unobservable", Source: "adapter-limitation", Connection: "unobservable", Inventory: "unobservable", Authorization: "unobservable"},
		ConfiguredSource: "compiled-configuration", IdentitySource: "unobservable", RequiredTools: slices.Clone(tools), InventorySource: "unobservable"}
}

func inspectMCPDiagnostics(ctx context.Context, session requiredMCPSession, reports []MCPDiagnostic, eligible []apiv1.MCPServer, settle time.Duration) []MCPDiagnostic {
	reports[0].MCPReadiness, _ = probeRequiredMCPSession(ctx, session, settle)
	reports[0].Source = "disposable-adapter-session"
	reports[0].Probe = "get_run_info"
	if reports[0].Connection == "ready" {
		reports[0].NegotiatedServer, reports[0].IdentitySource = goobersIOServerName, "adapter-session-registry"
	}
	if reports[0].Inventory == "ready" {
		reports[0].AvailableTools = slices.Clone(goobersIOTools)
		reports[0].InventorySource = "adapter-session-tool-inventory"
	}
	for i := 1; i < len(reports); i++ {
		if !slices.ContainsFunc(eligible, func(server apiv1.MCPServer) bool { return server.Name == reports[i].Server }) {
			reports[i].Source = "credential-scope-unobservable"
			continue
		}
		bounded, cancel := context.WithTimeout(ctx, requiredMCPProbeTimeout)
		reports[i] = observeExternalMCP(bounded, session, reports[i])
		cancel()
	}
	return reports
}

// observeExternalMCP only lists servers and tools. No execution operation is
// reachable here, including when a tool advertises itself as read-only (#6481).
func observeExternalMCP(ctx context.Context, session requiredMCPSession, report MCPDiagnostic) MCPDiagnostic {
	report.Source = "disposable-adapter-session"
	servers, err := session.ListMCP(ctx)
	if err != nil {
		report.Category = "transport_failure"
		if nativeMCPProbeUnsupported(err) {
			report.Category = "check_unobservable"
		}
		return report
	}
	if servers == nil {
		return report
	}
	if servers.Host != nil && slices.Contains(servers.Host.FilteredServers, report.Server) {
		report.Category, report.Authorization = "tool_authorization_failure", "denied"
		return report
	}
	for _, server := range servers.Servers {
		if server.Name == report.Server {
			report.NegotiatedServer, report.IdentitySource = server.Name, "adapter-session-registry"
			report.ObservedStatus = knownMCPServerStatus(server.Status)
			return observeExternalMCPTools(ctx, session, server.Status, report)
		}
	}
	report.Category = "required_server_unavailable"
	for _, server := range servers.Servers {
		if server.Name != goobersIOServerName && !slices.Contains(report.configuredServers, server.Name) {
			report.Category = "server_identity_mismatch"
		}
	}
	return report
}

func observeExternalMCPTools(ctx context.Context, session requiredMCPSession, status rpc.MCPServerStatus, report MCPDiagnostic) MCPDiagnostic {
	switch status {
	case rpc.MCPServerStatusNeedsAuth:
		report.Category, report.Authorization = "authentication_failure", "denied"
	case rpc.MCPServerStatusDisabled:
		report.Category, report.Authorization = "tool_authorization_failure", "denied"
	case rpc.MCPServerStatusConnected:
		report.Connection = "ready"
		return observeExternalMCPInventory(ctx, session, report)
	case rpc.MCPServerStatusFailed, rpc.MCPServerStatusStopped:
		report.Category = "transport_failure"
	default:
		report.Category = "check_unobservable"
	}
	return report
}

func observeExternalMCPInventory(ctx context.Context, session requiredMCPSession, report MCPDiagnostic) MCPDiagnostic {
	tools, err := session.ListMCPTools(ctx, report.Server)
	if err != nil {
		report.Category = "transport_failure"
		if nativeMCPProbeUnsupported(err) {
			report.Category = "check_unobservable"
		}
		return report
	}
	if tools == nil {
		return report
	}
	report.InventorySource = "adapter-session-tool-inventory"
	for _, tool := range tools.Tools {
		report.AvailableTools = append(report.AvailableTools, tool.Name)
	}
	slices.Sort(report.AvailableTools)
	report.Inventory, report.Category = "ready", "authorization_unobservable"
	for _, required := range report.RequiredTools {
		if !slices.Contains(report.AvailableTools, required) {
			report.MissingTools = append(report.MissingTools, required)
		}
	}
	if len(report.MissingTools) > 0 {
		report.Inventory, report.Category = "unavailable", "required_tool_unavailable"
		for _, missing := range report.MissingTools {
			if slices.Contains(report.AvailableTools, strings.TrimPrefix(missing, report.Server+"-")) && strings.HasPrefix(missing, report.Server+"-") {
				report.Category = "tool_alias_mismatch"
			}
		}
	}
	return report
}
