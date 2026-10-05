package harness

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

type diagnosticSession struct {
	readinessSession
	servers *rpc.MCPServerList
	tools   *rpc.MCPListToolsResult
	err     error
}

func (s *diagnosticSession) ListMCP(ctx context.Context) (*rpc.MCPServerList, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("unbounded observation")
	}
	return s.servers, s.err
}

func (s *diagnosticSession) ListMCPTools(_ context.Context, _ string) (*rpc.MCPListToolsResult, error) {
	return s.tools, s.err
}

func TestMCPDiagnosticsExternalInventoryNeverExecutesTools(t *testing.T) {
	for _, tc := range []struct {
		name, status, server, tool, required, category string
		missing, transport, unsupported                bool
	}{
		{name: "read only named mutation remains unobservable", status: "connected", server: "external", tool: "get_run_info", required: "get_run_info", category: "authorization_unobservable"},
		{name: "mutation remains unobservable", status: "connected", server: "external", tool: "delete_repository", required: "delete_repository", category: "authorization_unobservable"},
		{name: "missing tool", status: "connected", server: "external", tool: "other", required: "read", category: "required_tool_unavailable"},
		{name: "alias", status: "connected", server: "external", tool: "read", required: "external-read", category: "tool_alias_mismatch"},
		{name: "server alias", status: "connected", server: "alias", category: "server_identity_mismatch"},
		{name: "absent", missing: true, category: "required_server_unavailable"},
		{name: "authentication", status: "needs-auth", server: "external", category: "authentication_failure"},
		{name: "authorization", status: "disabled", server: "external", category: "tool_authorization_failure"},
		{name: "transport status", status: "failed", server: "external", category: "transport_failure"},
		{name: "transport RPC", transport: true, category: "transport_failure"},
		{name: "unsupported RPC", unsupported: true, category: "check_unobservable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &diagnosticSession{servers: &rpc.MCPServerList{Host: &rpc.MCPHostState{}}, tools: &rpc.MCPListToolsResult{Tools: []rpc.MCPTools{{Name: tc.tool}}}}
			if !tc.missing {
				s.servers.Servers = []rpc.MCPServer{{Name: tc.server, Status: rpc.MCPServerStatus(tc.status)}}
			}
			if tc.transport {
				s.err = errors.New("private token=secret")
			}
			if tc.unsupported {
				s.err = &readinessRPCError{Code: -32601}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			got := observeExternalMCP(ctx, s, configuredMCPDiagnostic("external", []string{tc.required}))
			if got.Category != tc.category || s.probeCalls != 0 || s.modelCalls != 0 {
				t.Fatalf("got=%+v calls=%d/%d", got, s.probeCalls, s.modelCalls)
			}
			data, _ := json.Marshal(got)
			if strings.Contains(string(data), "secret") {
				t.Fatal("leaked RPC error")
			}
		})
	}
}

func TestMCPDiagnosticsProbeHasNoModelTurnAndCleansWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name, category            string
		denied, unsupported, wait bool
	}{
		{"success", "ready", false, false, false},
		{"authorization", "tool_authorization_failure", true, false, false},
		{"unsupported", "check_unobservable", false, true, false},
		{"deadline", "transport_failure", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := &readinessSession{status: "connected", denied: tc.denied, unsupported: tc.unsupported, wait: tc.wait}
			workspace := ""
			adapter := &CopilotAdapter{Command: []string{"copilot"}, SelfBin: "/trusted/goobers", mcpSessionFactory: func(_ context.Context, request ProcessRequest, runner *copilotControlledRunner) (copilotModelSession, error) {
				workspace = request.Dir
				if !runner.diagnosticsOnly {
					t.Fatal("missing mutation denial policy")
				}
				return session, nil
			}}
			// Only the deadline case may see an expired caller context, and
			// it expires before the call, so no outcome depends on how long
			// setup takes on a loaded runner (#6617). The other cases run under
			// the probe's own MCPDiagnosticTimeout.
			ctx := context.Background()
			if tc.wait {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Unix(0, 0))
				defer cancel()
			}
			got := adapter.ProbeMCPReadiness(ctx, RunRequest{})
			if len(got) != 1 || got[0].Category != tc.category || session.modelCalls != 0 {
				t.Fatalf("got=%+v modelCalls=%d", got, session.modelCalls)
			}
			if tc.category == "ready" && session.probeCalls != 1 {
				t.Fatal("safe read was not executed exactly once")
			}
			if _, err := os.Stat(workspace); !os.IsNotExist(err) {
				t.Fatalf("workspace not cleaned: %v", err)
			}
		})
	}
}

func TestMCPDiagnosticsRefusesBuiltinImpersonationAndLaunchers(t *testing.T) {
	adapter := &CopilotAdapter{Command: []string{"copilot"}, SelfBin: "/trusted/goobers", mcpSessionFactory: func(context.Context, ProcessRequest, *copilotControlledRunner) (copilotModelSession, error) {
		t.Fatal("must not open a session")
		return nil, nil
	}}
	got := adapter.ProbeMCPReadiness(context.Background(), RunRequest{MCPServers: []apiv1.MCPServer{{Name: goobersIOServerName, Command: "untrusted"}}})
	if got[0].Category != "server_identity_mismatch" {
		t.Fatalf("got=%+v", got)
	}
	adapter.Command = []string{"custom-launcher", "copilot"}
	got = adapter.ProbeMCPReadiness(context.Background(), RunRequest{})
	if got[0].Category != "check_unobservable" {
		t.Fatalf("got=%+v", got)
	}
}

func TestMCPDiagnosticPermissionsIgnoreReadOnlyHintsAndAliases(t *testing.T) {
	yes := true
	runner := &copilotControlledRunner{diagnosticsOnly: true, request: RunRequest{Tools: append([]string{"shell", "external-get_run_info"}, goobersIOAvailableToolNames()...)}}
	permission := runner.sessionConfig("readiness", ProcessRequest{}, nil).OnPermissionRequest
	for _, request := range []copilot.PermissionRequest{
		rpc.PermissionRequestMCP{ServerName: "external", ToolName: "get_run_info", ReadOnly: yes},
		rpc.PermissionRequestMCP{ServerName: "goobers", ToolName: "io-get_run_info", ReadOnly: yes},
		rpc.PermissionRequestMCP{ServerName: goobersIOServerName, ToolName: "publish_output", ReadOnly: yes},
		rpc.PermissionRequestMCP{ServerName: goobersIOServerName, ToolName: "read_input", ReadOnly: yes},
		rpc.PermissionRequestShell{}, rpc.PermissionRequestWrite{},
	} {
		decision, err := permission(request, copilot.PermissionInvocation{})
		if _, allowed := decision.(*rpc.PermissionDecisionApproveOnce); allowed || err != nil {
			t.Fatalf("mutation allowed: %T: %v", request, err)
		}
	}
	decision, err := permission(rpc.PermissionRequestMCP{ServerName: goobersIOServerName, ToolName: "get_run_info"}, copilot.PermissionInvocation{})
	if _, allowed := decision.(*rpc.PermissionDecisionApproveOnce); !allowed || err != nil {
		t.Fatalf("safe builtin denied: %T %v", decision, err)
	}
}

func TestMCPDiagnosticsDoesNotSubstituteAmbientCredentials(t *testing.T) {
	session := &readinessSession{status: "connected"}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, SelfBin: "/trusted/goobers",
		ModelCredential: func(context.Context) (string, error) { t.Fatal("readiness resolved a model secret"); return "", nil },
		mcpSessionFactory: func(_ context.Context, _ ProcessRequest, runner *copilotControlledRunner) (copilotModelSession, error) {
			if len(runner.request.MCPServers) != 0 {
				t.Fatal("credentialed server launched without scoped credentials")
			}
			return session, nil
		}}
	reports := adapter.ProbeMCPReadiness(context.Background(), RunRequest{MCPServers: []apiv1.MCPServer{{Name: "external", URL: "https://example.invalid/mcp", CredentialRefs: []apiv1.MCPCredentialRef{{Kind: apiv1.MCPCredentialKindBYO, Ref: "external-token", Header: "Authorization"}}}}})
	if len(reports) != 2 || reports[1].Source != "credential-scope-unobservable" || reports[1].Category != "check_unobservable" || reports[1].Connection != "unobservable" {
		t.Fatalf("reports=%+v", reports)
	}
}

func TestMCPDiagnosticsDoesNotInferAliasFromAnotherConfiguredServer(t *testing.T) {
	reports := ConfiguredMCPDiagnostics([]apiv1.MCPServer{{Name: "absent"}, {Name: "present"}}, []string{"read"})
	s := &diagnosticSession{servers: &rpc.MCPServerList{Servers: []rpc.MCPServer{{Name: "present", Status: rpc.MCPServerStatusConnected}}}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got := observeExternalMCP(ctx, s, reports[1])
	if got.Category != "required_server_unavailable" {
		t.Fatalf("unrelated server was treated as alias: %+v", got)
	}
}

func TestMCPDiagnosticsSharedAllowlistDoesNotRequireToolsPerServer(t *testing.T) {
	for _, tc := range []struct {
		name        string
		allowlist   []string
		inventories map[string][]string
	}{
		{"mixed harness and external", []string{"shell", "read-context"}, map[string][]string{"context": {"read-context"}}},
		{"disjoint external servers", []string{"read-context", "search-docs"}, map[string][]string{"context": {"read-context"}, "docs": {"search-docs"}}},
		{"allowlist is not a requirement", []string{"optional-tool"}, map[string][]string{"external": {}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var configured []apiv1.MCPServer
			servers := &rpc.MCPServerList{}
			for name := range tc.inventories {
				configured = append(configured, apiv1.MCPServer{Name: name, URL: "https://example.invalid/mcp"})
				servers.Servers = append(servers.Servers, rpc.MCPServer{Name: name, Status: rpc.MCPServerStatusConnected})
			}
			reports := ConfiguredMCPDiagnostics(configured, tc.allowlist)
			if !slices.Equal(reports[0].RequiredTools, goobersIOTools) {
				t.Fatal("adapter-owned builtin requirements lost")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			for _, report := range reports[1:] {
				inventory := &rpc.MCPListToolsResult{}
				for _, name := range tc.inventories[report.Server] {
					inventory.Tools = append(inventory.Tools, rpc.MCPTools{Name: name})
				}
				session := &diagnosticSession{servers: servers, tools: inventory}
				got := observeExternalMCP(ctx, session, report)
				if got.Category != "authorization_unobservable" || got.Inventory != "ready" || len(got.RequiredTools) != 0 || len(got.MissingTools) != 0 {
					t.Fatalf("shared allowlist became a server requirement: %+v", got)
				}
				if !slices.Equal(got.ToolAllowlist, tc.allowlist) || !slices.Equal(got.AvailableTools, tc.inventories[report.Server]) {
					t.Fatalf("configured policy or observed inventory lost: %+v", got)
				}
				if session.probeCalls != 0 || session.modelCalls != 0 {
					t.Fatal("external inventory check executed a tool or model")
				}
			}
		})
	}
}
