package harness

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"

	"github.com/github/copilot-sdk/go/rpc"

	"github.com/goobers/goobers/internal/journal"
)

func (c *CopilotAdapter) prepareRequiredMCPRunner(req RunRequest, promptIndex int, config, model string, options map[string]string, confinement *copilotConfinement) (ProcessRunner, func()) {
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
		settleTimeout: c.RequiredMCPSettleTimeout,
		readiness:     MCPReadiness{Server: goobersIOServerName, Category: "transport_failure", Source: "adapter-session", Connection: "unobservable", Inventory: "unobservable", Authorization: "unobservable", ObservedStatus: mcpObservedUnobserved}}
	if confinement != nil {
		controlled.permissionRoots = append([]string(nil), confinement.writableRoots...)
	}
	return controlled, controlled.close
}

type mcpUnobservableRunner struct {
	base    ProcessRunner
	request RunRequest
	// source names why the check is unobservable; empty means the default
	// "adapter-limitation".
	source   string
	reported bool
}

func (r *mcpUnobservableRunner) Run(ctx context.Context, req ProcessRequest) (ProcessResult, error) {
	if !r.reported {
		r.reported = true
		source := r.source
		if source == "" {
			source = "adapter-limitation"
		}
		if err := emitMCPReadiness(r.request, MCPReadiness{Server: goobersIOServerName, Category: "check_unobservable", Source: source, Connection: "unobservable", Inventory: "unobservable", Authorization: "unobservable", ObservedStatus: mcpObservedUnobserved}); err != nil {
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
		return appender.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: stage, Runner: requiredMCPReadinessAnnotation(e.adapter.Name(), report)})
	}
}

// RequiredMCPReadinessSchemaVersion is the current required-mcp-readiness
// annotation schema. Version 2 adds the #5397 diagnostics; readers still
// accept version 1.
const RequiredMCPReadinessSchemaVersion = 2

// requiredMCPReadinessAnnotation builds the durable annotation. Every value is
// categorical or numeric: no server message, tool response or credential.
func requiredMCPReadinessAnnotation(adapter string, report MCPReadiness) map[string]any {
	observed := report.ObservedStatus
	if observed == "" {
		observed = mcpObservedUnobserved
	}
	return map[string]any{
		"kind": "required-mcp-readiness", "schemaVersion": RequiredMCPReadinessSchemaVersion, "adapter": adapter, "server": report.Server,
		"category": report.Category, "source": report.Source, "phase": "before-model", "connection": report.Connection,
		"inventory": report.Inventory, "authorization": report.Authorization, "observedStatus": observed, "polls": report.Polls,
		"elapsedMs": report.ElapsedMs, "failedReasonPresent": report.FailedReasonPresent,
	}
}

func readinessReportedError(req RunRequest, report MCPReadiness, err error) error {
	return errors.Join(err, emitMCPReadiness(req, report))
}

func withUnobservableMCP(base ProcessRunner, req RunRequest) ProcessRunner {
	return withUnobservableMCPSource(base, req, "")
}

// withUnobservableMCPSource is withUnobservableMCP with an explicit readiness
// source. The returned runner reports once however many invocations (initial
// turn, completion repair) it carries.
func withUnobservableMCPSource(base ProcessRunner, req RunRequest, source string) ProcessRunner {
	if req.GoobersIORegistered {
		return &mcpUnobservableRunner{base: base, request: req, source: source}
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
	blocked := copilotMCPEnterpriseBlockedServers(logPath)
	for _, name := range copilotRegisteredMCPServers(req) {
		status := controlledMCPFailureStatus(servers, name, controlled.readiness)
		// #6358: the session list cannot say why a server is missing; the
		// CLI's log can, and an enterprise lockdown wants its own action.
		if status != "" && status != copilotMCPStatusRemovedAfterConnect &&
			hasKey(blocked, name) {
			status = copilotMCPStatusEnterpriseBlocked
		}
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
