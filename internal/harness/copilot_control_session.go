package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

type copilotModelSession interface {
	requiredMCPSession
	RunPrompt(context.Context, string, ProcessRequest) (ProcessResult, error)
}

type copilotSessionFactory func(context.Context, ProcessRequest, *copilotControlledRunner) (copilotModelSession, error)

type sdkRequiredMCPSession struct{ session *copilot.Session }

func (s sdkRequiredMCPSession) RunPrompt(ctx context.Context, prompt string, req ProcessRequest) (ProcessResult, error) {
	return runControlledCopilotPrompt(ctx, s.session, prompt, req)
}

func (s sdkRequiredMCPSession) InitializeTools(ctx context.Context) error {
	_, err := s.session.RPC.Tools.InitializeAndValidate(ctx)
	return err
}
func (s sdkRequiredMCPSession) ListMCP(ctx context.Context) (*rpc.MCPServerList, error) {
	return s.session.RPC.MCP.List(ctx)
}
func (s sdkRequiredMCPSession) ListMCPTools(ctx context.Context, name string) (*rpc.MCPListToolsResult, error) {
	return s.session.RPC.MCP.ListTools(ctx, &rpc.MCPListToolsRequest{ServerName: name})
}
func (s sdkRequiredMCPSession) ExecuteTool(ctx context.Context, name string) (rpc.ToolResult, error) {
	return s.session.RPC.Tools.Execute(ctx, &rpc.ToolsExecuteRequest{Name: name, Arguments: map[string]any{}})
}

type copilotControlledRunner struct {
	base        ProcessRunner
	request     RunRequest
	promptIndex int
	mcpConfig   string
	model       string
	options     map[string]string
	process     *copilotControlProcess
	session     copilotModelSession
	factory     copilotSessionFactory
	ready       bool
	runCtx      context.Context
	runCancel   context.CancelFunc
	readiness   MCPReadiness
}

func (r *copilotControlledRunner) initialize(ctx context.Context, req ProcessRequest) error {
	process, id, err := startCopilotControlProcess(ctx, r.base, req, r.promptIndex)
	if err != nil {
		return err
	}
	r.process = process
	servers, err := copilotControlledServers(r.request, req.Env, r.mcpConfig)
	if err != nil {
		return err
	}
	config := r.sessionConfig(id, req, servers)
	startup, cancelStartup := context.WithTimeout(ctx, requiredMCPProbeTimeout)
	defer cancelStartup()
	session, err := process.client.CreateSession(startup, config)
	if err != nil {
		return fmt.Errorf("%w: create controlled Copilot session", errRequiredMCPUnavailable)
	}
	r.session = sdkRequiredMCPSession{session}
	return nil
}

func (r *copilotControlledRunner) Run(ctx context.Context, req ProcessRequest) (ProcessResult, error) {
	if r.runCtx == nil {
		r.runCtx, r.runCancel = copilotSessionContext(ctx, req.Timeout)
	}
	callCtx, cancel := copilotSessionContext(r.runCtx, req.Timeout)
	defer cancel()
	result, err := r.run(callCtx, req)
	if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
		err = errors.Join(ErrTimeout, err)
	}
	if errors.Is(callCtx.Err(), context.Canceled) {
		err = errors.Join(ErrCanceled, err)
	}
	return result, err
}

func (r *copilotControlledRunner) run(ctx context.Context, req ProcessRequest) (ProcessResult, error) {
	if !r.ready {
		err := r.open(r.runCtx, req)
		if err == nil {
			r.readiness, err = probeRequiredMCPSession(ctx, r.session)
		}
		if err := readinessReportedError(r.request, r.readiness, err); err != nil {
			return ProcessResult{ExitCode: -1}, err
		}
		r.ready = true
	}
	prompt, err := copilotControlledPrompt(req.Command, r.promptIndex)
	if err != nil {
		return ProcessResult{ExitCode: -1}, err
	}
	return r.session.RunPrompt(ctx, prompt, req)
}

func (r *copilotControlledRunner) open(ctx context.Context, req ProcessRequest) error {
	if r.factory == nil {
		return r.initialize(ctx, req)
	}
	var err error
	r.session, err = r.factory(ctx, req, r)
	return err
}

func (r *copilotControlledRunner) close() {
	if r.runCancel != nil {
		r.runCancel()
	}
	if r.process != nil {
		r.process.close()
	}
}

func copilotControlledPrompt(argv []string, index int) (string, error) {
	if index < 0 || index >= len(argv) {
		return "", fmt.Errorf("missing controlled Copilot prompt")
	}
	for i, b := range []byte(argv[index]) {
		if b == '=' {
			return argv[index][i+1:], nil
		}
	}
	return "", fmt.Errorf("controlled Copilot prompt must be bound to its flag")
}

func runControlledCopilotPrompt(ctx context.Context, session *copilot.Session, prompt string, req ProcessRequest) (ProcessResult, error) {
	buffer := newTranscriptBuffer(req.MaxTranscriptBytes)
	tracker := newActivityTracker(time.Now())
	buffer.progress = func() { tracker.observe(time.Now()) }
	sampler := startActivitySampler(ctx, tracker, req.ActivityInterval, buffer.observedBytes, req.Activity)
	defer sampler.stop()
	checkpoints := startTranscriptCheckpoints(buffer, req.TranscriptCheckpointInterval, req.TranscriptCheckpoint)
	unsubscribe := session.On(func(event copilot.SessionEvent) {
		if data, err := json.Marshal(event); err == nil {
			_, _ = buffer.Write(append(data, '\n'))
		}
	})
	final, err := session.SendAndWait(ctx, copilot.MessageOptions{Prompt: prompt})
	unsubscribe()
	if final != nil && req.StdoutCapture != nil {
		if message, ok := final.Data.(*rpc.AssistantMessageData); ok {
			_, _ = req.StdoutCapture.Write([]byte(message.Content))
		}
	}
	timedOut, canceled := errors.Is(ctx.Err(), context.DeadlineExceeded), errors.Is(ctx.Err(), context.Canceled)
	err = errors.Join(err, checkpoints.finish(transcriptEndReason(timedOut, canceled)))
	if timedOut {
		err = errors.Join(ErrTimeout, err)
	} else if canceled {
		err = errors.Join(ErrCanceled, err)
	}
	result := ProcessResult{Transcript: buffer.Bytes(), TranscriptTruncated: buffer.Truncated(), TranscriptDroppedBytes: buffer.Dropped()}
	if err != nil {
		result.ExitCode = -1
	}
	return result, err
}

func copilotControlledServers(req RunRequest, env []string, additional string) (map[string]copilot.MCPServerConfig, error) {
	paths := []string{additional}
	if len(req.MCPServers) > 0 {
		home, ok := copilotConfigHome(env)
		if !ok {
			return nil, fmt.Errorf("scoped MCP home unavailable")
		}
		paths = append(paths, filepath.Join(home, "mcp-config.json"))
	}
	servers := make(map[string]copilot.MCPServerConfig)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read prepared MCP registration: %w", err)
		}
		var config copilotMCPConfig
		if err := json.Unmarshal(data, &config); err != nil {
			return nil, fmt.Errorf("decode prepared MCP registration: %w", err)
		}
		for name, server := range config.MCPServers {
			if server.Type == "http" {
				servers[name] = copilot.MCPHTTPServerConfig{URL: server.URL, Tools: server.Tools, Headers: server.Headers}
				continue
			}
			servers[name] = copilot.MCPStdioServerConfig{Command: server.Command, Args: server.Args, Env: server.Env, Tools: server.Tools, WorkingDirectory: req.Workspace}
		}
	}
	return servers, nil
}

func (r *copilotControlledRunner) sessionConfig(id string, req ProcessRequest, servers map[string]copilot.MCPServerConfig) *copilot.SessionConfig {
	config := &copilot.SessionConfig{
		SessionID: id, Model: r.model, ReasoningEffort: r.options["reasoningEffort"],
		WorkingDirectory: req.Dir, MCPServers: servers, AvailableTools: copilotAvailableTools(r.request),
		OnPermissionRequest: copilotSessionPermissions(r.request),
	}
	if r.options["context"] == "long_context" {
		config.ContextTier = copilot.ContextTierLongContext
	}
	if home, ok := copilotConfigHome(req.Env); ok {
		config.ConfigDirectory = home
	}
	if copilotDeclaresTool(r.request.Tools, "github") {
		config.GitHubMCPToolConfig = &copilot.GitHubMCPToolConfig{AdditionalToolsets: []string{"issues"}}
	} else if len(r.request.MCPServers) > 0 {
		config.DisabledMCPServers = []string{"github-mcp-server"}
	}
	return config
}
