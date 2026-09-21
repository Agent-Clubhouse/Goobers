package harness

import (
	"context"
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
}

func (s *readinessSession) InitializeTools(ctx context.Context) error {
	if s.wait {
		<-ctx.Done()
		return ctx.Err()
	}
	if s.transport {
		return errors.New("private transport token=secret")
	}
	return ctx.Err()
}
func (s *readinessSession) ListMCP(context.Context) (*rpc.MCPServerList, error) {
	if s.status == "absent" {
		return &rpc.MCPServerList{}, nil
	}
	return &rpc.MCPServerList{Servers: []rpc.MCPServer{{Name: goobersIOServerName, Status: rpc.MCPServerStatus(s.status)}}}, nil
}
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
		_, err := runner.Run(context.Background(), ProcessRequest{Command: []string{"copilot", "-p=work"}, Dir: t.TempDir()})
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

func TestRequiredMCPReadinessHonorsInvocationDeadline(t *testing.T) {
	session := &readinessSession{wait: true}
	runner := &copilotControlledRunner{promptIndex: 1, factory: func(context.Context, ProcessRequest, *copilotControlledRunner) (copilotModelSession, error) {
		return session, nil
	}}
	defer runner.close()
	_, err := runner.Run(context.Background(), ProcessRequest{Command: []string{"copilot", "-p=work"}, Timeout: 10 * time.Millisecond})
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
	_, err := runner.Run(context.Background(), ProcessRequest{Command: []string{"copilot", "-p=work"}})
	if !errors.Is(err, failure) || session.modelCalls != 0 {
		t.Fatalf("calls=%d error=%v", session.modelCalls, err)
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
