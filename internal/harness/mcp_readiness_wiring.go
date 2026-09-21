package harness

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"

	"github.com/github/copilot-sdk/go/rpc"

	"github.com/goobers/goobers/internal/journal"
)

func (c *CopilotAdapter) prepareRequiredMCPRunner(req RunRequest, promptIndex int, config, model string, options map[string]string) (ProcessRunner, func()) {
	base := c.runner()
	if !req.GoobersIORegistered {
		return base, func() {}
	}
	// Custom launchers and process adapters do not promise the headless RPC
	// contract. Preserve their execution path while making that limitation
	// explicit; a disposable external probe must not masquerade as evidence.
	direct := len(c.Command) == 1 && filepath.Base(c.Command[0]) == "copilot"
	if !direct || c.Runner != nil && c.mcpSessionFactory == nil || c.RequireLauncherContract || c.ExtraArgs != nil || runtime.GOOS == "windows" && c.mcpSessionFactory == nil {
		return &mcpUnobservableRunner{base: base, request: req}, func() {}
	}
	controlled := &copilotControlledRunner{base: base, request: req, promptIndex: promptIndex, mcpConfig: config, model: model, options: options, factory: c.mcpSessionFactory,
		readiness: MCPReadiness{Server: goobersIOServerName, Category: "transport_failure", Source: "adapter-session", Connection: "unobservable", Inventory: "unobservable", Authorization: "unobservable"}}
	return controlled, controlled.close
}

type mcpUnobservableRunner struct {
	base     ProcessRunner
	request  RunRequest
	reported bool
}

func (r *mcpUnobservableRunner) Run(ctx context.Context, req ProcessRequest) (ProcessResult, error) {
	if !r.reported {
		r.reported = true
		if err := emitMCPReadiness(r.request, MCPReadiness{Server: goobersIOServerName, Category: "check_unobservable", Source: "adapter-limitation", Connection: "unobservable", Inventory: "unobservable", Authorization: "unobservable"}); err != nil {
			return ProcessResult{ExitCode: -1}, err
		}
	}
	return r.base.Run(ctx, req)
}

func emitMCPReadiness(req RunRequest, report MCPReadiness) error {
	if req.MCPReadinessSink == nil {
		return nil
	}
	return req.MCPReadinessSink(report)
}

func (e *Executor) mcpReadinessSink(stage string) func(MCPReadiness) error {
	appender, ok := e.recorder.(EventAppender)
	if !ok {
		return nil
	}
	return func(report MCPReadiness) error {
		return appender.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: stage, Runner: map[string]any{
			"kind": "required-mcp-readiness", "schemaVersion": 1, "adapter": e.adapter.Name(), "server": report.Server, "category": report.Category, "source": report.Source, "phase": "before-model", "connection": report.Connection, "inventory": report.Inventory, "authorization": report.Authorization}})
	}
}

func readinessReportedError(req RunRequest, report MCPReadiness, err error) error {
	return errors.Join(err, emitMCPReadiness(req, report))
}

func withUnobservableMCP(base ProcessRunner, req RunRequest) ProcessRunner {
	if req.GoobersIORegistered {
		return &mcpUnobservableRunner{base: base, request: req}
	}
	return base
}

func (e *Executor) prepareReadinessRequest(req *RunRequest) {
	req.MCPReadinessSink = e.mcpReadinessSink(req.Envelope.TaskID)
	if req.Attempt < 1 {
		req.Attempt = 1
	}
}

// The CLI-global lifecycle log does not describe SDK-created sessions. Query
// the actual session after its turn; retain log evidence for ordinary launchers.
func copilotRunnerMCPFailures(ctx context.Context, runner ProcessRunner, req RunRequest, logPath string) []MCPServerFailure {
	controlled, ok := runner.(*copilotControlledRunner)
	if !ok {
		return copilotMCPServerFailures(req, logPath)
	}
	if controlled.session == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, requiredMCPProbeTimeout)
	defer cancel()
	servers, err := controlled.session.ListMCP(ctx)
	if err != nil || servers == nil {
		return nil
	} // Unknown does not prove absence.
	var failures []MCPServerFailure
	for _, name := range copilotRegisteredMCPServers(req) {
		status := controlledMCPFailureStatus(servers, name, controlled.readiness)
		if status != "" {
			failures = append(failures, MCPServerFailure{Server: name, Status: status})
		}
	}
	return failures
}

func controlledMCPFailureStatus(servers *rpc.MCPServerList, name string, readiness MCPReadiness) string {
	for _, server := range servers.Servers {
		if server.Name != name {
			continue
		}
		switch server.Status {
		case "connected":
			return ""
		case "disabled", "needs-auth":
			return copilotMCPStatusPolicyRejected
		default:
			return copilotMCPStatusHandshakeIncomplete
		}
	}
	if name == readiness.Server && readiness.Connection == "ready" {
		return copilotMCPStatusRemovedAfterConnect
	}
	return copilotMCPStatusAbsent
}
