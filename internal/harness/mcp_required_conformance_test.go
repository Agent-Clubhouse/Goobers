package harness

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/github/copilot-sdk/go/rpc"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow"
)

type mcpRequirednessRunner struct {
	adapter     string
	modelTurns  int
	failStartup bool
}

type mcpRequirednessCopilotSession struct {
	runner *mcpRequirednessRunner
}

func (s *mcpRequirednessCopilotSession) InitializeTools(context.Context) error { return nil }
func (s *mcpRequirednessCopilotSession) ListMCP(context.Context) (*rpc.MCPServerList, error) {
	return &rpc.MCPServerList{
		Host: &rpc.MCPHostState{},
		Servers: []rpc.MCPServer{{
			Name:   "context",
			Status: rpc.MCPServerStatusFailed,
		}},
	}, nil
}
func (s *mcpRequirednessCopilotSession) ListMCPTools(context.Context, string) (*rpc.MCPListToolsResult, error) {
	return nil, nil
}
func (s *mcpRequirednessCopilotSession) ExecuteTool(context.Context, string) (rpc.ToolResult, error) {
	return nil, nil
}
func (s *mcpRequirednessCopilotSession) RunPrompt(_ context.Context, _ string, req ProcessRequest) (ProcessResult, error) {
	s.runner.modelTurns++
	return ProcessResult{}, WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
}

func (r *mcpRequirednessRunner) Run(ctx context.Context, req ProcessRequest) (ProcessResult, error) {
	switch r.adapter {
	case "claude-code":
		init := []byte(`{"type":"system","subtype":"init","mcp_servers":[{"name":"context","status":"failed"}]}` + "\n")
		if req.StdoutCapture != nil {
			_, _ = req.StdoutCapture.Write(init)
		}
		result := ProcessResult{Transcript: init}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		r.modelTurns++
		if err := WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}); err != nil {
			return result, err
		}
		terminal := []byte(`{"type":"result","subtype":"success","result":"done"}` + "\n")
		if req.StdoutCapture != nil {
			_, _ = req.StdoutCapture.Write(terminal)
		}
		result.Transcript = append(result.Transcript, terminal...)
		return result, nil
	case "codex":
		if r.failStartup {
			stderr := []byte("Failed to initialize session: required MCP servers failed to initialize: context: failed to start\n")
			return ProcessResult{ExitCode: 1, Stderr: stderr}, errors.New("codex exited before model dispatch")
		}
		r.modelTurns++
		events := []byte("{\"type\":\"thread.started\",\"thread_id\":\"thread-1\"}\n{\"type\":\"turn.completed\",\"usage\":{}}\n")
		if req.StdoutCapture != nil {
			_, _ = req.StdoutCapture.Write(events)
		}
		if err := WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}); err != nil {
			return ProcessResult{}, err
		}
		return ProcessResult{
			Transcript: events,
			Stderr:     []byte("MCP client for `context` failed to start\n"),
		}, nil
	case "copilot-cli":
		r.modelTurns++
		if err := WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}); err != nil {
			return ProcessResult{}, err
		}
		return ProcessResult{Transcript: []byte(`{"status":"success","summary":"done"}`)}, nil
	default:
		return ProcessResult{}, fmt.Errorf("unknown adapter %q", r.adapter)
	}
}

func TestConformanceMCPServerRequiredness(t *testing.T) {
	for _, ca := range conformanceAdapters() {
		t.Run(ca.name, func(t *testing.T) {
			ca.stub(t)
			for _, tc := range []struct {
				name        string
				required    *bool
				dslVersion  string
				legacy      bool
				wantFailure bool
				wantWarning bool
			}{
				{name: "required-fail", required: mcpRequiredFlag(true), dslVersion: "3.1", wantFailure: true},
				{name: "optional-fail", required: mcpRequiredFlag(false), dslVersion: "3.1", wantWarning: true},
				{name: "default-omitted-is-required", dslVersion: "3.1", wantFailure: true},
				{name: "older-pin-keeps-adapter-semantics", dslVersion: "3.0", legacy: true, wantWarning: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					workspace := t.TempDir()
					wantFailure := tc.wantFailure || tc.legacy && ca.name == "codex"
					runner := &mcpRequirednessRunner{adapter: ca.name, failStartup: wantFailure}
					adapter := ca.build(runner, "")
					if copilot, ok := adapter.(*CopilotAdapter); ok {
						copilot.mcpSessionFactory = func(context.Context, ProcessRequest, *copilotControlledRunner) (copilotModelSession, error) {
							return &mcpRequirednessCopilotSession{runner: runner}, nil
						}
					}
					server := apiv1.MCPServer{
						Name:     "context",
						Required: tc.required,
						Command:  "context-server",
					}
					if tc.dslVersion == "3.1" {
						spec := workflow.MaterializeGooberForDefinition(
							workflow.Definition{DSLVersion: "3.1"},
							apiv1.GooberSpec{MCPServers: []apiv1.MCPServer{server}},
						)
						server = spec.MCPServers[0]
					}
					out, err := adapter.Run(context.Background(), RunRequest{
						Envelope:       testEnvelope(workspace, "agent:model"),
						Workspace:      workspace,
						CompletionPath: DefaultResultPath,
						Credentials:    mcpTestCredentials(t, "agent:model", "model-token"),
						MCPServers:     []apiv1.MCPServer{server},
					})
					if wantFailure {
						wantClass := errRequiredMCPUnavailable
						if tc.legacy && ca.name == "codex" {
							wantClass = errRequiredMCPDeclaredStartupFailed
						}
						if !errors.Is(err, wantClass) {
							t.Fatalf("Run error = %v, want required-MCP startup failure", err)
						}
						if runner.modelTurns != 0 {
							t.Fatalf("model turns = %d, want none before required-server failure", runner.modelTurns)
						}
						return
					}
					if err != nil {
						t.Fatalf("Run: %v", err)
					}
					if runner.modelTurns != 1 {
						t.Fatalf("model turns = %d, want one for optional-server failure", runner.modelTurns)
					}
					if tc.wantWarning && !wantFailure &&
						(len(out.MCPServerFailures) != 1 || out.MCPServerFailures[0].Server != "context") {
						t.Fatalf("MCP failures = %+v, want optional-server warning evidence", out.MCPServerFailures)
					}
				})
			}
		})
	}
}
