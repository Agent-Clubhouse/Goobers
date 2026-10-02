package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// MCPDiagnosticTimeout bounds the complete disposable session, independently
// of the stage's execution budget. Each observation also has a phase deadline.
const MCPDiagnosticTimeout = 60 * time.Second

// ProbeMCPReadiness starts the supported direct CLI control protocol, never
// Adapter.Run or Preflight (which can send a model prompt). Only the built-in
// get_run_info is executed. External servers have inventory-only observations.
// The caller must not treat these local-process facts as worker attestation.
func (c *CopilotAdapter) ProbeMCPReadiness(ctx context.Context, req RunRequest) []MCPDiagnostic {
	reports := ConfiguredMCPDiagnostics(req.MCPServers, req.Tools)
	// An external declaration cannot replace the adapter-owned safe probe.
	if slices.ContainsFunc(req.MCPServers, func(server apiv1.MCPServer) bool { return server.Name == goobersIOServerName }) {
		return failedMCPDiagnostics(reports, "server_identity_mismatch")
	}
	if !c.supportsMCPDiagnostics() {
		return reports
	}
	ctx, cancel := context.WithTimeout(ctx, MCPDiagnosticTimeout)
	defer cancel()
	workspace, err := os.MkdirTemp("", "goobers-mcp-readiness-")
	if err != nil {
		return failedMCPDiagnostics(reports, "transport_failure")
	}
	defer func() { _ = os.RemoveAll(workspace) }()
	req.Workspace, req.Envelope.Workspace = workspace, workspace
	req.Envelope.RunID = "readiness"
	req.ContextPaths, req.Envelope.Inputs = nil, nil
	// Credentialed external servers cannot substitute ambient credentials for
	// their declared grant. Keep them unobservable until scoped credentials
	// are supplied by the caller; do not resolve model credentials here.
	var eligible = req.MCPServers[:0:0]
	for i, server := range req.MCPServers {
		if len(server.CredentialRefs) == 0 || req.Credentials != nil {
			eligible = append(eligible, server)
		} else {
			reports[i+1].Source = "credential-scope-unobservable"
		}
	}
	req.MCPServers = eligible
	req = withAutoGoobersIO(req, c.SelfBin)
	config, err := goobersIOAdditionalMCPConfigArg(req, c.SelfBin)
	if err != nil {
		return failedMCPDiagnostics(reports, "transport_failure")
	}
	env, _, cleanup, err := prepareCopilotPreflightEnvironment(baseEnv(c.ExtraEnvAllowlist, c.EnvUnset), false)
	if err != nil {
		return failedMCPDiagnostics(reports, "transport_failure")
	}
	defer cleanup()
	env, err = prepareCopilotMCP(ctx, req, env)
	if err != nil {
		return failedMCPDiagnostics(reports, "authentication_failure")
	}
	id, err := newHarnessSessionID()
	if err != nil {
		return failedMCPDiagnostics(reports, "transport_failure")
	}
	runner := &copilotControlledRunner{base: c.runner(), request: req, promptIndex: 1, mcpConfig: config, factory: c.mcpSessionFactory, diagnosticsOnly: true}
	defer runner.close()
	process := ProcessRequest{Command: []string{c.Command[0], "-p=", "--session-id", id, "--log-dir", filepath.Join(workspace, "logs")}, Dir: workspace, Env: env, Timeout: MCPDiagnosticTimeout}
	if err := runner.open(ctx, process); err != nil {
		return failedMCPDiagnostics(reports, "transport_failure")
	}
	return inspectMCPDiagnostics(ctx, runner.session, reports, eligible, c.RequiredMCPSettleTimeout)
}

func (c *CopilotAdapter) supportsMCPDiagnostics() bool {
	return len(c.Command) == 1 && filepath.Base(c.Command[0]) == "copilot" && c.SelfBin != "" &&
		!c.RequireLauncherContract && c.ExtraArgs == nil &&
		(c.Runner == nil || c.mcpSessionFactory != nil) && (runtime.GOOS != "windows" || c.mcpSessionFactory != nil)
}

func failedMCPDiagnostics(reports []MCPDiagnostic, category string) []MCPDiagnostic {
	for i := range reports {
		if reports[i].Source == "credential-scope-unobservable" {
			continue
		}
		reports[i].Category = category
		reports[i].Source = "disposable-adapter-session"
	}
	return reports
}

// diagnosticMCPPermissions denies every mutation and every external tool call,
// regardless of tool names or server-supplied safety annotations.
func diagnosticMCPPermissions() copilot.PermissionHandlerFunc {
	allowed := copilotSessionPermissions(RunRequest{Tools: []string{goobersIOServerName + "-get_run_info"}}, nil)
	return func(request copilot.PermissionRequest, invocation copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
		if request == nil || request.Kind() != rpc.PermissionRequestKindMCP {
			return &rpc.PermissionDecisionUserNotAvailable{}, nil
		}
		var detail copilotPermissionDetail
		data, err := json.Marshal(request)
		if err != nil || json.Unmarshal(data, &detail) != nil || detail.ServerName != goobersIOServerName ||
			(detail.ToolName != "get_run_info" && detail.ToolName != goobersIOServerName+"-get_run_info") {
			return &rpc.PermissionDecisionUserNotAvailable{}, nil
		}
		return allowed(request, invocation)
	}
}
