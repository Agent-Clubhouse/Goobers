package harness

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/github/copilot-sdk/go/rpc"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

type readinessSession struct {
	status      string
	denied      bool
	missingTool bool
	transport   bool
	unsupported bool
	wait        bool
	modelCalls  int
	probeCalls  int
	// starting lists the startup snapshots ListMCP reports, in order, before
	// the settled status: "pending", "pending-connection" or
	// "host-uninitialized" (#5397).
	starting  []string
	listCalls int
	// failedReason makes the host record a goobers-io connection failure
	// whose message must never reach the annotation.
	failedReason bool
	// connecting keeps goobers-io in the host's in-flight connections while
	// it is listed with status.
	connecting bool
	// initDeadline and listDeadline record the deadline of the context each
	// phase received.
	initDeadline, listDeadline time.Time
}

func (s *readinessSession) InitializeTools(ctx context.Context) error {
	s.initDeadline, _ = ctx.Deadline()
	if s.wait {
		<-ctx.Done()
		return ctx.Err()
	}
	if s.transport {
		return errors.New("private transport token=secret")
	}
	return ctx.Err()
}
func (s *readinessSession) ListMCP(ctx context.Context) (*rpc.MCPServerList, error) {
	s.listDeadline, _ = ctx.Deadline()
	status := s.status
	if s.listCalls < len(s.starting) {
		status = s.starting[s.listCalls]
	}
	s.listCalls++
	host := &rpc.MCPHostState{}
	if s.failedReason {
		host.FailedServers = map[string]rpc.MCPServerFailureInfo{goobersIOServerName: {Message: readinessFailureMessage}}
	}
	if s.connecting {
		host.PendingConnections = []string{goobersIOServerName}
	}
	switch status {
	case "host-uninitialized":
		return &rpc.MCPServerList{}, nil
	case "absent":
		return &rpc.MCPServerList{Host: host}, nil
	case "pending-connection":
		host.PendingConnections = []string{goobersIOServerName}
		return &rpc.MCPServerList{Host: host}, nil
	}
	return &rpc.MCPServerList{Host: host, Servers: []rpc.MCPServer{{Name: goobersIOServerName, Status: rpc.MCPServerStatus(status)}}}, nil
}

// readinessFailureMessage stands in for a server failure text that could
// carry a credential; it must never appear in a readiness annotation.
const readinessFailureMessage = "connect failed token=ghp_secretvalue"

func (s *readinessSession) ListMCPTools(context.Context, string) (*rpc.MCPListToolsResult, error) {
	tools := &rpc.MCPListToolsResult{}
	for _, name := range goobersIOTools {
		if s.missingTool && name == "publish_output" {
			continue
		}
		tools.Tools = append(tools.Tools, rpc.MCPTools{Name: name})
	}
	return tools, nil
}
func (s *readinessSession) ExecuteTool(_ context.Context, name string) (rpc.ToolResult, error) {
	if name != goobersIOServerName+"-get_run_info" {
		return nil, errors.New("mutating or inspection probe")
	}
	s.probeCalls++
	if s.unsupported {
		return nil, &readinessRPCError{Code: -32601}
	}
	if s.denied {
		return rpc.ToolResultExpanded{ResultType: rpc.ToolResultTypeDenied}, nil
	}
	return rpc.ToolResultExpanded{ResultType: rpc.ToolResultTypeSuccess}, nil
}
func (s *readinessSession) RunPrompt(_ context.Context, _ string, req ProcessRequest) (ProcessResult, error) {
	s.modelCalls++
	if s.probeCalls == 0 {
		return ProcessResult{}, errors.New("model dispatched before read-only probe")
	}
	return ProcessResult{}, WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
}

type readinessRPCError struct {
	Code int `json:"code"`
}

func (*readinessRPCError) Error() string { return "unsupported RPC" }

func TestRequiredMCPReadinessPrecedesActualAdapterModelDispatch(t *testing.T) {
	for _, tc := range []struct {
		name     string
		session  readinessSession
		category string
		infra    bool
		calls    int
	}{
		{name: "ready", session: readinessSession{status: "connected"}, category: "ready", calls: 1},
		{name: "legacy native authorization unobservable", session: readinessSession{status: "connected", unsupported: true}, category: "check_unobservable", calls: 1},
		{name: "absent", session: readinessSession{status: "absent"}, category: "required_tool_unavailable", infra: true},
		{name: "starting then connected", session: readinessSession{status: "connected", starting: []string{"host-uninitialized", "pending", "pending"}}, category: "ready", calls: 1},
		{name: "failed", session: readinessSession{status: "failed"}, category: "required_tool_unavailable", infra: true},
		{name: "connected unauthorized", session: readinessSession{status: "connected", denied: true}, category: "tool_authorization_failure"},
		{name: "authentication", session: readinessSession{status: "needs-auth"}, category: "authentication_failure"},
		{name: "missing required tool", session: readinessSession{status: "connected", missingTool: true}, category: "required_tool_unavailable", infra: true},
		{name: "transport", session: readinessSession{transport: true}, category: "transport_failure", infra: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &fakeRecorder{}
			adapter := &CopilotAdapter{Command: []string{"copilot"}, SelfBin: "/test/goobers", Runner: &fakeProcessRunner{act: func(ProcessRequest) error { t.Fatal("unprobed process dispatched"); return nil }},
				mcpSessionFactory: func(context.Context, ProcessRequest, *copilotControlledRunner) (copilotModelSession, error) {
					return &tc.session, nil
				}}
			executor, err := NewExecutor(adapter, testInjector(t, "", "", noopRegistrar{}), rec, rec, rec, journal.NewPatternScrubber(), "")
			if err != nil {
				t.Fatal(err)
			}
			result, err := executor.Invoke(context.Background(), testEnvelope(t.TempDir()))
			if tc.calls == 1 && err != nil {
				t.Fatalf("ready invocation: %v", err)
			}
			if tc.calls == 0 && err == nil {
				t.Fatalf("unready invocation succeeded: %+v", result)
			}
			if invoke.IsInfrastructureFailure(err) != tc.infra {
				t.Fatalf("infrastructure=%v error=%v", invoke.IsInfrastructureFailure(err), err)
			}
			if tc.session.modelCalls != tc.calls {
				t.Fatalf("model calls=%d want%d", tc.session.modelCalls, tc.calls)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatal("probe leaked transport details")
			}
			found := false
			for _, event := range rec.events {
				if event.Runner["kind"] == "required-mcp-readiness" {
					found = true
					if tc.session.unsupported && (event.Runner["connection"] != "ready" || event.Runner["inventory"] != "ready" || event.Runner["authorization"] != "unobservable") {
						t.Fatalf("partial readiness=%+v", event.Runner)
					}
					if event.Runner["category"] != tc.category || event.Runner["phase"] != "before-model" {
						t.Fatalf("readiness=%+v", event.Runner)
					}
				}
			}
			if !found {
				t.Fatal("missing durable readiness report")
			}
		})
	}
}

func TestRequiredMCPReadinessTransientRetryDoesNotSpendModelTurn(t *testing.T) {
	unavailable := &readinessSession{status: "absent"}
	available := &readinessSession{status: "connected"}
	var reports []MCPReadiness
	request := RunRequest{MCPReadinessSink: func(report MCPReadiness) error { reports = append(reports, report); return nil }}
	for _, session := range []*readinessSession{unavailable, available} {
		runner := &copilotControlledRunner{request: request, promptIndex: 1, readiness: MCPReadiness{Server: goobersIOServerName, Source: "adapter-session"}, factory: func(context.Context, ProcessRequest, *copilotControlledRunner) (copilotModelSession, error) {
			return session, nil
		}}
		_, err := runner.Run(context.Background(), ProcessRequest{Command: []string{"copilot", "-p="}, Stdin: []byte("work"), Dir: t.TempDir()})
		if session == unavailable && !errors.Is(err, errRequiredMCPUnavailable) {
			t.Fatalf("first attempt=%v", err)
		}
		if session == available && err != nil {
			t.Fatal(err)
		}
	}
	if unavailable.modelCalls != 0 || available.modelCalls != 1 || len(reports) != 2 {
		t.Fatalf("calls=%d,%d reports=%v", unavailable.modelCalls, available.modelCalls, reports)
	}
}

// #5397: a server that never leaves startup within the probe window is a
// bounded, retryable infrastructure failure reported before any model turn.
func TestRequiredMCPReadinessStartupThatNeverSettlesIsBoundedInfrastructure(t *testing.T) {
	for _, stuck := range []string{"pending", "host-uninitialized"} {
		t.Run(stuck, func(t *testing.T) {
			session := &readinessSession{status: stuck}
			ctx, cancel := context.WithTimeout(context.Background(), 3*requiredMCPSettlePoll)
			defer cancel()
			started := time.Now()
			report, err := probeRequiredMCPSession(ctx, session, 0)
			if !errors.Is(err, errRequiredMCPUnavailable) || report.Category != "required_tool_unavailable" || report.Connection != "unobservable" {
				t.Fatalf("report=%+v error=%v", report, err)
			}
			if session.listCalls < 2 || session.probeCalls != 0 {
				t.Fatalf("list calls=%d probe calls=%d; startup was not re-observed or was probed", session.listCalls, session.probeCalls)
			}
			if elapsed := time.Since(started); elapsed > requiredMCPProbeTimeout {
				t.Fatalf("probe outlived its window: %s", elapsed)
			}
		})
	}
}

func TestRequiredMCPReadinessHonorsInvocationDeadline(t *testing.T) {
	session := &readinessSession{wait: true}
	runner := &copilotControlledRunner{promptIndex: 1, factory: func(context.Context, ProcessRequest, *copilotControlledRunner) (copilotModelSession, error) {
		return session, nil
	}}
	defer runner.close()
	_, err := runner.Run(context.Background(), ProcessRequest{Command: []string{"copilot", "-p="}, Stdin: []byte("work"), Timeout: 10 * time.Millisecond})
	if !errors.Is(err, ErrTimeout) || session.modelCalls != 0 {
		t.Fatalf("calls=%d error=%v", session.modelCalls, err)
	}
}

func TestRequiredMCPReadinessReportFailurePreventsModel(t *testing.T) {
	failure := errors.New("journal unavailable")
	session := &readinessSession{status: "connected"}
	runner := &copilotControlledRunner{request: RunRequest{MCPReadinessSink: func(MCPReadiness) error { return failure }}, promptIndex: 1, factory: func(context.Context, ProcessRequest, *copilotControlledRunner) (copilotModelSession, error) {
		return session, nil
	}}
	defer runner.close()
	_, err := runner.Run(context.Background(), ProcessRequest{Command: []string{"copilot", "-p="}, Stdin: []byte("work")})
	if !errors.Is(err, failure) || session.modelCalls != 0 {
		t.Fatalf("calls=%d error=%v", session.modelCalls, err)
	}
}

// #5552: the SDK-controlled session must make the same GitHub MCP
// registration choice as the argv path, or write-capable cloud stages keep
// the read-only issue subset.
func TestCopilotControlledSessionGitHubRegistrationFollowsCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name         string
		capabilities []string
		wantAll      bool
	}{
		{name: "no capabilities", wantAll: false},
		{name: "issue read", capabilities: []string{"github:issues:read"}, wantAll: false},
		{name: "issue write", capabilities: []string{"telemetry:read", "github:issues:write"}, wantAll: true},
		{name: "issue approve", capabilities: []string{"github:issues:approve"}, wantAll: true},
		{name: "milestone write", capabilities: []string{"github:milestones:write"}, wantAll: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &copilotControlledRunner{request: RunRequest{
				Envelope: apiv1.InvocationEnvelope{Capabilities: tc.capabilities},
				Tools:    []string{"github"},
			}}
			got := runner.sessionConfig("session", ProcessRequest{Dir: "workspace"}, nil).GitHubMCPToolConfig
			if got == nil {
				t.Fatal("GitHub MCP tool config missing for a declared github group")
			}
			if tc.wantAll {
				if got.EnableAllTools == nil || !*got.EnableAllTools || len(got.AdditionalToolsets) != 0 {
					t.Fatalf("config = %+v, want EnableAllTools", got)
				}
				return
			}
			if got.EnableAllTools != nil || !reflect.DeepEqual(got.AdditionalToolsets, []string{"issues"}) {
				t.Fatalf("config = %+v, want the read-only issues toolset", got)
			}
		})
	}
}

func TestCopilotControlledSessionPreservesSettingsAndSandbox(t *testing.T) {
	home := filepath.Join(t.TempDir(), "copilot-home")
	runner := &copilotControlledRunner{model: "claude-sonnet-5", options: map[string]string{"context": "long_context", "reasoningEffort": "xhigh"}, request: RunRequest{Tools: append([]string{"github"}, goobersIOAvailableToolNames()...)}}
	config := runner.sessionConfig("owned-session", ProcessRequest{Dir: "workspace", Env: []string{"COPILOT_HOME=" + home}}, nil)
	if config.SessionID != "owned-session" || config.Model != runner.model || string(config.ContextTier) != "long_context" || config.ReasoningEffort != "xhigh" || config.ConfigDirectory != home || config.WorkingDirectory != "workspace" || !reflect.DeepEqual(config.AvailableTools, copilotAvailableTools(runner.request)) || config.GitHubMCPToolConfig.AdditionalToolsets[0] != "issues" {
		t.Fatalf("config=%+v", config)
	}
	command, id, err := copilotControlCommand([]string{"sandbox-exec", "-f", "profile", "copilot", "-p=private prompt", "--session-id", "owned-session", "--silent", "--output-format=text", "--allow-all-tools"}, 4)
	if err != nil || id != "owned-session" || !reflect.DeepEqual(command, []string{"sandbox-exec", "-f", "profile", "copilot", "--allow-all-tools", "--headless", "--no-auto-update", "--port", "0"}) {
		t.Fatalf("command=%q id=%s error=%v", command, id, err)
	}
}

// #5636: Copilot CLI refuses --usage-output-file with --headless, so the
// control command must drop the flag and its value in either spelling.
func TestCopilotControlCommandDropsUsageOutputFile(t *testing.T) {
	for name, usage := range map[string][]string{
		"spaced": {"--usage-output-file", ".goobers/copilot-usage-1/usage.json"},
		"equals": {"--usage-output-file=.goobers/copilot-usage-1/usage.json"},
	} {
		t.Run(name, func(t *testing.T) {
			argv := append([]string{"copilot", "-p=private prompt", "--session-id", "owned-session", "--allow-all-tools"}, usage...)
			argv = append(argv, "--log-dir", "logs")
			command, id, err := copilotControlCommand(argv, 1)
			want := []string{"copilot", "--allow-all-tools", "--log-dir", "logs", "--headless", "--no-auto-update", "--port", "0"}
			if err != nil || id != "owned-session" || !reflect.DeepEqual(command, want) {
				t.Fatalf("command=%q id=%s error=%v", command, id, err)
			}
		})
	}
}

// #5636: the CLI names a rejected argv on stderr and exits in about a second;
// the readiness error must carry that reason, redacted and bounded.
func TestCopilotControlExitBeforeReadinessReportsStatusAndStderr(t *testing.T) {
	secret := "ghp_" + strings.Repeat("x", 36)
	stderr := strings.Repeat("noise line\n", 1000) + "token " + secret + "\n" +
		"error: option '--usage-output-file <file>' cannot be used with option '--headless'\n"
	runner := &fakeProcessRunner{result: ProcessResult{ExitCode: 1, Stderr: []byte(stderr)}, err: errors.New("exit status 1")}
	_, _, err := startCopilotControlProcess(context.Background(), runner, ProcessRequest{
		Command: []string{"copilot", "-p=", "--session-id", "owned-session"},
		Stdin:   []byte("private prompt"),
	}, 1)
	if !errors.Is(err, errRequiredMCPUnavailable) {
		t.Fatalf("error=%v", err)
	}
	// The headless server takes the prompt over session RPC, never on stdin.
	if len(runner.lastReq.Stdin) != 0 {
		t.Fatalf("control process stdin = %q, want none", runner.lastReq.Stdin)
	}
	message := err.Error()
	if !strings.Contains(message, "exited before readiness (exit 1)") ||
		!strings.Contains(message, "cannot be used with option '--headless'") {
		t.Fatalf("error omitted exit status or stderr: %v", err)
	}
	if strings.Contains(message, secret) || !strings.Contains(message, journal.Redacted) {
		t.Fatalf("error did not scrub stderr: %v", err)
	}
	if !strings.Contains(message, "truncated") || len(message) > copilotControlExitDetailBytes+512 {
		t.Fatalf("error was not bounded: len=%d", len(message))
	}
}

func TestCopilotControlPortWaitsForCompleteLine(t *testing.T) {
	capture := &copilotPortCapture{ready: make(chan int, 1)}
	_, _ = capture.Write([]byte("listening on port 12"))
	select {
	case port := <-capture.ready:
		t.Fatalf("partial port=%d", port)
	default:
	}
	_, _ = capture.Write([]byte("345\n"))
	select {
	case port := <-capture.ready:
		if port != 12345 {
			t.Fatalf("port=%d", port)
		}
	default:
		t.Fatal("missing complete port")
	}
}

func TestControlledMCPPostTurnUsesActualSession(t *testing.T) {
	session := &readinessSession{status: "connected"}
	runner := &copilotControlledRunner{session: session, readiness: MCPReadiness{Server: goobersIOServerName, Connection: "ready"}}
	req := RunRequest{GoobersIORegistered: true}
	if failures := copilotRunnerMCPFailures(context.Background(), runner, req, "unrelated-global-log"); len(failures) != 0 {
		t.Fatalf("connected failures=%+v", failures)
	}
	session.status = "absent"
	failures := copilotRunnerMCPFailures(context.Background(), runner, req, "")
	if len(failures) != 1 || failures[0].Status != copilotMCPStatusRemovedAfterConnect {
		t.Fatalf("removed failures=%+v", failures)
	}
}

func TestRequiredMCPUnobservableAdapterReportsBeforeDispatch(t *testing.T) {
	reported := false
	req := RunRequest{GoobersIORegistered: true, MCPReadinessSink: func(report MCPReadiness) error {
		if report.Category != "check_unobservable" || report.Connection != "unobservable" || report.Authorization != "unobservable" {
			t.Fatalf("report=%+v", report)
		}
		reported = true
		return nil
	}}
	runner := withUnobservableMCP(&fakeProcessRunner{act: func(ProcessRequest) error {
		if !reported {
			t.Fatal("dispatched without observation limitation")
		}
		return nil
	}}, req)
	if _, err := runner.Run(context.Background(), ProcessRequest{}); err != nil {
		t.Fatal(err)
	}
}

// #5397: the durable annotation says which startup sub-state a check ended in
// without carrying the host's failure message.
func TestRequiredMCPReadinessAnnotationCarriesScrubbedDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name     string
		session  readinessSession
		observed string
		polls    int
		failed   bool
	}{
		{name: "ready", session: readinessSession{status: "connected"}, observed: "connected", polls: 1},
		{name: "settled after startup", session: readinessSession{status: "connected", starting: []string{"host-uninitialized", "pending-connection", "pending"}}, observed: "connected", polls: 4},
		{name: "failed with recorded reason", session: readinessSession{status: "failed", failedReason: true}, observed: "failed", polls: 1, failed: true},
		{name: "absent from initialized host", session: readinessSession{status: "absent"}, observed: "absent", polls: 1},
		{name: "unrecognized status", session: readinessSession{status: "private-status token=secret"}, observed: "unknown", polls: 1},
		{name: "transport before listing", session: readinessSession{transport: true}, observed: "unobserved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &fakeRecorder{}
			adapter := &CopilotAdapter{Command: []string{"copilot"}, SelfBin: "/test/goobers", Runner: &fakeProcessRunner{},
				mcpSessionFactory: func(context.Context, ProcessRequest, *copilotControlledRunner) (copilotModelSession, error) {
					return &tc.session, nil
				}}
			executor, err := NewExecutor(adapter, testInjector(t, "", "", noopRegistrar{}), rec, rec, rec, journal.NewPatternScrubber(), "")
			if err != nil {
				t.Fatal(err)
			}
			_, _ = executor.Invoke(context.Background(), testEnvelope(t.TempDir()))
			var annotation map[string]any
			for _, event := range rec.events {
				if event.Runner["kind"] == "required-mcp-readiness" {
					annotation = event.Runner
				}
			}
			if annotation == nil {
				t.Fatal("missing durable readiness report")
			}
			if annotation["schemaVersion"] != RequiredMCPReadinessSchemaVersion || annotation["observedStatus"] != tc.observed ||
				annotation["polls"] != tc.polls || annotation["failedReasonPresent"] != tc.failed {
				t.Fatalf("annotation=%+v", annotation)
			}
			if elapsed, ok := annotation["elapsedMs"].(int64); !ok || elapsed < 0 {
				t.Fatalf("elapsedMs=%#v", annotation["elapsedMs"])
			}
			encoded, err := json.Marshal(annotation)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "connect failed") {
				t.Fatalf("annotation leaked server detail: %s", encoded)
			}
		})
	}
}

// #5397: a goobers-io connection still in flight on the MCP host is startup,
// not a settled failure, whether or not the server is listed yet.
func TestRequiredMCPPendingConnectionIsStarting(t *testing.T) {
	connecting := &rpc.MCPHostState{PendingConnections: []string{goobersIOServerName}}
	listed := func(status rpc.MCPServerStatus, host *rpc.MCPHostState) *rpc.MCPServerList {
		return &rpc.MCPServerList{Host: host, Servers: []rpc.MCPServer{{Name: goobersIOServerName, Status: status}}}
	}
	for name, tc := range map[string]struct {
		servers  *rpc.MCPServerList
		starting bool
	}{
		"unlisted, connection in flight":   {servers: &rpc.MCPServerList{Host: connecting}, starting: true},
		"failed, reconnection in flight":   {servers: listed(rpc.MCPServerStatusFailed, connecting), starting: true},
		"connected, stale in-flight entry": {servers: listed(rpc.MCPServerStatusConnected, connecting)},
		"unlisted, other server in flight": {servers: &rpc.MCPServerList{Host: &rpc.MCPHostState{PendingConnections: []string{"other"}}}},
		"failed, no connection in flight":  {servers: listed(rpc.MCPServerStatusFailed, &rpc.MCPHostState{})},
		"pending without host state":       {servers: listed(rpc.MCPServerStatusPending, nil), starting: true},
		"host uninitialized":               {servers: &rpc.MCPServerList{}, starting: true},
		"no list":                          {},
	} {
		if got := requiredMCPStarting(tc.servers); got != tc.starting {
			t.Errorf("%s: starting=%v want %v", name, got, tc.starting)
		}
	}
	session := &readinessSession{status: "connected", starting: []string{"pending-connection", "pending-connection"}}
	report, err := probeRequiredMCPSession(context.Background(), session, 0)
	if err != nil || report.Category != "ready" || report.Polls != 3 || report.ObservedStatus != "connected" {
		t.Fatalf("report=%+v error=%v", report, err)
	}
	stuck := &readinessSession{status: "pending-connection"}
	report, err = probeRequiredMCPSession(context.Background(), stuck, 3*requiredMCPSettlePoll)
	if !errors.Is(err, errRequiredMCPUnavailable) || report.Category != "required_tool_unavailable" || report.ObservedStatus != "pending-connection" || report.Polls < 2 {
		t.Fatalf("report=%+v error=%v", report, err)
	}
}

// #5397: the settle wait has its own budget. InitializeTools keeps its bounded
// phase, the configured budget ends a startup that never settles with the
// unchanged error class, and zero or negative values select the default.
func TestRequiredMCPSettleBudgetIsSeparateAndBounded(t *testing.T) {
	if requiredMCPSettleBudget(0) != DefaultRequiredMCPSettleTimeout || requiredMCPSettleBudget(-time.Second) != DefaultRequiredMCPSettleTimeout || requiredMCPSettleBudget(time.Minute) != time.Minute {
		t.Fatal("settle budget resolution")
	}
	if DefaultRequiredMCPSettleTimeout <= requiredMCPProbeTimeout {
		t.Fatalf("default settle budget %s must exceed the %s phase cap", DefaultRequiredMCPSettleTimeout, requiredMCPProbeTimeout)
	}
	session := &readinessSession{status: "connected"}
	if _, err := probeRequiredMCPSession(context.Background(), session, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if session.initDeadline.IsZero() || session.listDeadline.IsZero() || session.listDeadline.Sub(session.initDeadline) < time.Hour {
		t.Fatalf("settle wait shares the initialization deadline: init=%s list=%s", session.initDeadline, session.listDeadline)
	}
	for _, stuck := range []string{"pending", "host-uninitialized"} {
		session := &readinessSession{status: stuck}
		report, err := probeRequiredMCPSession(context.Background(), session, 3*requiredMCPSettlePoll)
		if !errors.Is(err, errRequiredMCPUnavailable) || report.Category != "required_tool_unavailable" || session.probeCalls != 0 || report.Polls < 2 || report.Polls != session.listCalls {
			t.Fatalf("%s: report=%+v calls=%d error=%v", stuck, report, session.listCalls, err)
		}
	}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, RequiredMCPSettleTimeout: time.Minute, mcpSessionFactory: func(context.Context, ProcessRequest, *copilotControlledRunner) (copilotModelSession, error) {
		return session, nil
	}}
	runner, closeRunner := adapter.prepareRequiredMCPRunner(RunRequest{GoobersIORegistered: true}, 1, "", "", nil, nil)
	defer closeRunner()
	if controlled, ok := runner.(*copilotControlledRunner); !ok || controlled.settleTimeout != time.Minute {
		t.Fatalf("adapter settle budget not carried to the session runner: %#v", runner)
	}
}
