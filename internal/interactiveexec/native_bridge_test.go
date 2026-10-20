//go:build !windows

package interactiveexec

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/interactive"
	"github.com/goobers/goobers/internal/interactiveagent"
	auth "github.com/goobers/goobers/internal/interactiveauth"
	"github.com/goobers/goobers/internal/invoke"
)

func bridgeExecutor(t *testing.T) (*Executor, interactive.ExecutionRequest) {
	t.Helper()
	principal, _ := auth.LocalPrincipal("bridge-owner")
	request := interactive.ExecutionRequest{SessionID: "bridge-session", TurnID: "bridge-turn", Owner: "bridge-owner", Fence: 1, Attempt: 1, LeaseID: "bridge-lease", ExecutionMode: "codex-local", Mode: "helper", Harness: "codex", Model: "configured-model", Gaggle: "configured-gaggle", Goober: "configured-helper", AuthorityRefs: []string{"demo-ado"}, Tools: []string{"ado_work_item"}}
	broker, err := auth.Open(auth.Config{CheckExecution: func(_ context.Context, actor auth.Identity, operation auth.OperationRequest) error {
		if actor.Subject != request.Owner || operation.SessionID != request.SessionID || operation.TurnID != request.TurnID || operation.LeaseID != request.LeaseID || operation.ExecutionFence != request.Fence {
			return fmt.Errorf("stale server lease")
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = broker.Close() })
	refs := map[auth.Provider]auth.ConnectionRef{}
	for _, provider := range []auth.Provider{auth.ADO, auth.AppInsights} {
		connection, err := broker.ConnectDemo(context.Background(), principal, provider)
		if err != nil {
			t.Fatal(err)
		}
		refs[provider] = connection.Ref
	}
	e, err := New(Config{Sandbox: fixtureSandbox{}, StateDir: t.TempDir(), SourceDir: t.TempDir(), Mode: "local-tools", Model: "legacy-model", SelfBinary: "/trusted/host/goobers", Broker: broker, Principal: principal, TrustedLocalModelLogin: true, ResolveAuthority: func(_ context.Context, req interactive.ExecutionRequest, provider auth.Provider) (auth.ConnectionRef, error) {
		for _, name := range req.AuthorityRefs {
			if name == "demo-ado" && provider == auth.ADO || name == "demo-insights" && provider == auth.AppInsights {
				return refs[provider], nil
			}
		}
		return auth.ConnectionRef{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	// The native integration tests inject every process/harness callback. They
	// never execute Codex, a model, git, node, or another child process.
	e.runProcess = func(_ context.Context, _ string, command []string, _ func(harness.TranscriptDelta) error) (harness.ProcessResult, error) {
		if strings.Join(command, " ") == "git rev-parse HEAD" {
			return harness.ProcessResult{Transcript: []byte(strings.Repeat("a", 40))}, nil
		}
		return harness.ProcessResult{}, nil
	}
	return e, request
}

type bridgeRPC struct {
	conn   net.Conn
	reader *bufio.Reader
}

func connectBridge(path string) (*bridgeRPC, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, err
	}
	token, err := os.ReadFile(filepath.Join(filepath.Dir(path), "auth.token"))
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err = conn.Write(token); err != nil {
		_ = conn.Close()
		return nil, err
	}
	var authenticated [3]byte
	if _, err = io.ReadFull(conn, authenticated[:]); err != nil || string(authenticated[:]) != "OK\n" {
		_ = conn.Close()
		return nil, fmt.Errorf("bridge authentication failed")
	}
	client := &bridgeRPC{conn: conn, reader: bufio.NewReader(conn)}
	if _, err = client.request(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return client, nil
}
func (c *bridgeRPC) request(request string) (map[string]any, error) {
	_ = c.conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(c.conn, request+"\n"); err != nil {
		return nil, err
	}
	line, err := c.reader.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err = json.Unmarshal(line, &result); err != nil {
		return nil, err
	}
	if result["error"] != nil {
		return nil, fmt.Errorf("RPC failed")
	}
	return result, nil
}
func bridgeSocket(request harness.RunRequest) (string, error) {
	if len(request.Tools) != 0 {
		return "", fmt.Errorf("MCP tools were incorrectly supplied as Codex built-in-tool restrictions")
	}
	if len(request.MCPServers) != 1 {
		return "", fmt.Errorf("expected one invocation-bound MCP server")
	}
	server := request.MCPServers[0]
	if server.Command != "/trusted/host/goobers" || len(server.Args) != 5 || server.Args[0] != "agent-tools" || server.Args[1] != "--socket" || server.Args[3] != "--token-file" || server.Args[4] != filepath.Join(filepath.Dir(server.Args[2]), "auth.token") {
		return "", fmt.Errorf("untrusted MCP command")
	}
	return server.Args[2], nil
}
func successOutcome() harness.Outcome {
	return harness.Outcome{Payload: []byte(`{"status":"success","summary":"Controlled harness completed."}`)}
}
func sampleAgentTools(invokeTool func(context.Context, interactiveagent.Call) (any, error)) interactiveagent.Options {
	return interactiveagent.Options{Tools: []interactiveagent.Tool{{Name: "ado_work_item", Description: "Read the configured ADO work item.", InputSchema: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"key": map[string]any{"type": "string"}, "workItemId": map[string]any{"type": "integer"}}, "required": []string{"key", "workItemId"}}}, {Name: "hidden_tool", Description: "Not selected by the pinned policy.", InputSchema: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}}}}, Check: func(ctx context.Context) error { return ctx.Err() }, Invoke: invokeTool}
}

func TestNativeHelperUsesPinnedModelAndScopedRealMCPWithoutCannedInterception(t *testing.T) {
	e, req := bridgeExecutor(t)
	req.Text = "Could you investigate the surprising invoice behavior and explain which evidence matters?"
	req.Instructions = "Use the configured investigation persona."
	var helperCalls, toolCalls atomic.Int32
	socket := ""
	e.config.Helper = func(context.Context, interactive.ExecutionRequest, func(interactive.ExecutionEvent) error) (string, error) {
		helperCalls.Add(1)
		return "canned helper", nil
	}
	e.config.AgentTools = func(_ context.Context, bound interactive.ExecutionRequest, _ string, _ func(string, map[string]any) error) (interactiveagent.Options, error) {
		return sampleAgentTools(func(ctx context.Context, call interactiveagent.Call) (any, error) {
			toolCalls.Add(1)
			var args struct {
				Key        string `json:"key"`
				WorkItemID int    `json:"workItemId"`
			}
			if json.Unmarshal(call.Arguments, &args) != nil {
				return nil, fmt.Errorf("bad args")
			}
			return e.ReadProvider(ctx, bound, args.Key, auth.ReadWorkItem, auth.Payload{WorkItemID: args.WorkItemID})
		}), nil
	}
	e.runNative = func(ctx context.Context, run harness.RunRequest) (harness.Outcome, error) {
		ack := invoke.RegisterWorkspaceWriter(ctx)
		defer ack(nil)
		if run.Model != req.Model || run.Envelope.Gaggle != req.Gaggle || run.Envelope.Goober != req.Goober || !strings.HasPrefix(run.Envelope.Goal, req.Text+"\n") || !strings.Contains(run.Instructions, req.Instructions) {
			return harness.Outcome{}, fmt.Errorf("pinned model/persona/natural text changed")
		}
		var err error
		socket, err = bridgeSocket(run)
		if err != nil {
			return harness.Outcome{}, err
		}
		client, err := connectBridge(socket)
		if err != nil {
			return harness.Outcome{}, err
		}
		defer func() {
			if err := client.conn.Close(); err != nil {
				t.Error(err)
			}
		}()
		list, err := client.request(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
		if err != nil {
			return harness.Outcome{}, err
		}
		encoded, _ := json.Marshal(list)
		if strings.Contains(string(encoded), "hidden_tool") || !strings.Contains(string(encoded), "ado_work_item") {
			return harness.Outcome{}, fmt.Errorf("pinned tool exposure ignored")
		}
		result, err := client.request(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ado_work_item","arguments":{"key":"read-427","workItemId":427}}}`)
		if err != nil {
			return harness.Outcome{}, err
		}
		encoded, _ = json.Marshal(result)
		if !strings.Contains(string(encoded), "controlled-local") || strings.Contains(string(encoded), `"isError":true`) {
			return harness.Outcome{}, fmt.Errorf("scoped provider tool failed")
		}
		return successOutcome(), nil
	}
	result, err := e.Execute(context.Background(), req, func(interactive.ExecutionEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if result.Message != "Controlled harness completed." || helperCalls.Load() != 0 || toolCalls.Load() != 1 {
		t.Fatalf("wrong execution path: %+v helper=%d tools=%d", result, helperCalls.Load(), toolCalls.Load())
	}
	if _, err = os.Stat(socket); !os.IsNotExist(err) {
		t.Fatal("bridge survived invocation completion")
	}
}

func TestProviderBindingsAreExplicitExactAndCannotWiden(t *testing.T) {
	e, req := bridgeExecutor(t)
	ctx := context.Background()
	none := req
	none.AuthorityRefs = nil
	if _, err := e.providerContext(ctx, none, func(string, map[string]any) error { return nil }); err != nil {
		t.Fatal(err)
	}
	authorities, _ := e.config.Broker.ListAuthorities(e.config.Principal)
	if len(authorities) != 0 {
		t.Fatal("absence of authority created blanket provider grant")
	}
	if _, err := e.ReadProvider(ctx, none, "no-auth", auth.ReadWorkItem, auth.Payload{WorkItemID: 427}); err == nil {
		t.Fatal("explicit provider tool ignored no-authority policy")
	}
	if _, err := e.ReadProvider(ctx, req, "ado", auth.ReadWorkItem, auth.Payload{WorkItemID: 427}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ReadProvider(ctx, req, "azure", auth.QueryTelemetry, auth.Payload{Query: "requests | summarize count() by resultCode"}); err == nil {
		t.Fatal("ADO-only policy gained Azure access")
	}
	if _, err := e.ReadProvider(ctx, req, "write", auth.AddComment, auth.Payload{WorkItemID: 427, Comment: "not permitted"}); err == nil {
		t.Fatal("model read adapter accepted mutation")
	}
	changed := req
	changed.AuthorityRefs = []string{"demo-ado", "demo-insights"}
	if _, err := e.ReadProvider(ctx, changed, "widen", auth.QueryTelemetry, auth.Payload{Query: "requests | summarize count() by resultCode"}); err == nil {
		t.Fatal("existing session authority widened after configuration change")
	}
	changed = req
	changed.Owner = "other"
	if _, err := e.ReadProvider(ctx, changed, "owner", auth.ReadWorkItem, auth.Payload{WorkItemID: 427}); err == nil {
		t.Fatal("cross-owner read admitted")
	}
	changed = req
	changed.ChildID = "child"
	if _, err := e.ReadProvider(ctx, changed, "child", auth.ReadWorkItem, auth.Payload{WorkItemID: 427}); err == nil {
		t.Fatal("child used parent model provider interface")
	}
	changed = req
	changed.LeaseID = "stale"
	if _, err := e.ReadProvider(ctx, changed, "lease", auth.ReadWorkItem, auth.Payload{WorkItemID: 427}); auth.ErrorCode(err) != "execution_unavailable" {
		t.Fatalf("stale lease accepted: %v", err)
	}
	if _, err := e.ReadProvider(ctx, req, "ado", auth.ReadWorkItem, auth.Payload{WorkItemID: 428}); auth.ErrorCode(err) != "idempotency_conflict" {
		t.Fatalf("changed stable intent replayed: %v", err)
	}
	ops, _ := e.config.Broker.ListOperations(e.config.Principal, req.SessionID)
	if len(ops) != 1 || ops[0].Resource != auth.DemoADOResource() {
		t.Fatalf("unexpected provider effects: %+v", ops)
	}
}

func TestResolverMustReturnOwnedExactProviderConnection(t *testing.T) {
	e, req := bridgeExecutor(t)
	foreign, _ := auth.LocalPrincipal("foreign")
	connection, err := e.config.Broker.ConnectDemo(context.Background(), foreign, auth.ADO)
	if err != nil {
		t.Fatal(err)
	}
	e.config.ResolveAuthority = func(context.Context, interactive.ExecutionRequest, auth.Provider) (auth.ConnectionRef, error) {
		return connection.Ref, nil
	}
	if _, err = e.ReadProvider(context.Background(), req, "foreign", auth.ReadWorkItem, auth.Payload{WorkItemID: 427}); err == nil {
		t.Fatal("foreign connection binding accepted")
	}
	e.config.ResolveAuthority = nil
	if _, err = e.ReadProvider(context.Background(), req, "missing", auth.ReadWorkItem, auth.Payload{WorkItemID: 427}); err == nil {
		t.Fatal("missing resolver fell back to configured application access")
	}
}

func TestNativeSteeringRetiresOldBridgeBeforeAcknowledgment(t *testing.T) {
	e, req := bridgeExecutor(t)
	workspace := t.TempDir()
	steering := make(chan interactive.Steering, 1)
	req.Steering = steering
	first := make(chan string, 1)
	var calls atomic.Int32
	e.config.AgentTools = func(context.Context, interactive.ExecutionRequest, string, func(string, map[string]any) error) (interactiveagent.Options, error) {
		return sampleAgentTools(func(context.Context, interactiveagent.Call) (any, error) { return "ok", nil }), nil
	}
	e.runNative = func(ctx context.Context, run harness.RunRequest) (harness.Outcome, error) {
		ack := invoke.RegisterWorkspaceWriter(ctx)
		defer ack(nil)
		socket, err := bridgeSocket(run)
		if err != nil {
			return harness.Outcome{}, err
		}
		if calls.Add(1) == 1 {
			first <- socket
			<-ctx.Done()
			return harness.Outcome{}, harness.ErrCanceled
		}
		if !strings.Contains(run.Envelope.Goal, "follow the second approach") {
			return harness.Outcome{}, fmt.Errorf("steering text lost")
		}
		return successOutcome(), nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := e.native(context.Background(), req, workspace, "", func(string, map[string]any) error { return nil })
		done <- err
	}()
	oldSocket := <-first
	acknowledged := make(chan error, 1)
	steering <- interactive.Steering{Text: "follow the second approach", Ack: func(err error) {
		if _, statErr := os.Stat(oldSocket); !os.IsNotExist(statErr) {
			err = fmt.Errorf("old bridge still exists at steering acknowledgment")
		}
		acknowledged <- err
	}}
	if err := <-acknowledged; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("steering did not create exactly one fresh invocation")
	}
}

func TestNativeStopClosesBridgeAndWaitsForToolCallbacks(t *testing.T) {
	e, req := bridgeExecutor(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	toolStopped := make(chan struct{})
	socket := ""
	e.config.AgentTools = func(context.Context, interactive.ExecutionRequest, string, func(string, map[string]any) error) (interactiveagent.Options, error) {
		return sampleAgentTools(func(ctx context.Context, _ interactiveagent.Call) (any, error) {
			close(entered)
			<-ctx.Done()
			close(toolStopped)
			return nil, ctx.Err()
		}), nil
	}
	e.runNative = func(ctx context.Context, run harness.RunRequest) (harness.Outcome, error) {
		ack := invoke.RegisterWorkspaceWriter(ctx)
		defer ack(nil)
		var err error
		socket, err = bridgeSocket(run)
		if err != nil {
			return harness.Outcome{}, err
		}
		client, err := connectBridge(socket)
		if err != nil {
			return harness.Outcome{}, err
		}
		defer func() {
			if err := client.conn.Close(); err != nil {
				t.Error(err)
			}
		}()
		_, _ = io.WriteString(client.conn, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ado_work_item","arguments":{"key":"stop","workItemId":427}}}`+"\n")
		<-ctx.Done()
		return harness.Outcome{}, harness.ErrCanceled
	}
	done := make(chan error, 1)
	go func() {
		_, err := e.native(ctx, req, t.TempDir(), "", func(string, map[string]any) error { return nil })
		done <- err
	}()
	<-entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("native stop did not finish")
	}
	select {
	case <-toolStopped:
	default:
		t.Fatal("stop returned before host callback stopped")
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatal("socket survived stop")
	}
}

func TestNativeUnstoppedHostToolPreventsSteeringAcknowledgmentAndRestart(t *testing.T) {
	e, req := bridgeExecutor(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	steering := make(chan interactive.Steering, 1)
	req.Steering = steering
	var calls atomic.Int32
	e.config.AgentTools = func(context.Context, interactive.ExecutionRequest, string, func(string, map[string]any) error) (interactiveagent.Options, error) {
		options := sampleAgentTools(func(context.Context, interactiveagent.Call) (any, error) {
			close(entered)
			<-release
			return "late", nil
		})
		options.ShutdownTimeout = 30 * time.Millisecond
		return options, nil
	}
	e.runNative = func(ctx context.Context, run harness.RunRequest) (harness.Outcome, error) {
		calls.Add(1)
		ack := invoke.RegisterWorkspaceWriter(ctx)
		defer ack(nil)
		socket, err := bridgeSocket(run)
		if err != nil {
			return harness.Outcome{}, err
		}
		client, err := connectBridge(socket)
		if err != nil {
			return harness.Outcome{}, err
		}
		defer func() {
			if err := client.conn.Close(); err != nil {
				t.Error(err)
			}
		}()
		_, _ = io.WriteString(client.conn, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ado_work_item","arguments":{"key":"pending","workItemId":427}}}`+"\n")
		<-ctx.Done()
		return harness.Outcome{}, harness.ErrCanceled
	}
	done := make(chan error, 1)
	result := make(chan interactive.ExecutionResult, 1)
	go func() {
		out, err := e.Execute(context.Background(), req, func(interactive.ExecutionEvent) error { return nil })
		result <- out
		done <- err
	}()
	<-entered
	acknowledged := make(chan error, 1)
	steering <- interactive.Steering{Text: "continue", Ack: func(err error) { acknowledged <- err }}
	if err := <-acknowledged; !errors.Is(err, interactiveagent.ErrNotQuiescent) {
		t.Fatalf("host callback uncertainty acknowledged as stopped: %v", err)
	}
	if err := <-done; !errors.Is(err, invoke.ErrWorkspaceNotQuiescent) {
		t.Fatalf("lost custody failure: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("native invocation restarted with old host callback still live")
	}
	if out := <-result; !out.CustodyPending {
		t.Fatal("host callback uncertainty did not retain session custody")
	}
	if err := e.Recover(context.Background(), req); err == nil {
		t.Fatal("recovery forgot the unconfirmed host callback")
	}
}

func TestPinnedUnsupportedHarnessCannotFallBack(t *testing.T) {
	e, req := bridgeExecutor(t)
	req.Harness = "unsupported"
	var calls atomic.Int32
	e.runNative = func(context.Context, harness.RunRequest) (harness.Outcome, error) {
		calls.Add(1)
		return successOutcome(), nil
	}
	if _, err := e.Execute(context.Background(), req, func(interactive.ExecutionEvent) error { return nil }); err == nil || calls.Load() != 0 {
		t.Fatal("unsupported harness silently fell back")
	}
}

func TestNoConfiguredToolsMeansNoNativeMCPBridge(t *testing.T) {
	e, req := bridgeExecutor(t)
	req.Tools = nil
	var toolsCalled atomic.Int32
	e.config.AgentTools = func(context.Context, interactive.ExecutionRequest, string, func(string, map[string]any) error) (interactiveagent.Options, error) {
		toolsCalled.Add(1)
		return interactiveagent.Options{}, nil
	}
	e.runNative = func(ctx context.Context, run harness.RunRequest) (harness.Outcome, error) {
		ack := invoke.RegisterWorkspaceWriter(ctx)
		defer ack(nil)
		if len(run.MCPServers) != 0 || len(run.Tools) != 0 {
			return harness.Outcome{}, fmt.Errorf("unconfigured tool exposure")
		}
		return successOutcome(), nil
	}
	if _, err := e.native(context.Background(), req, t.TempDir(), "", func(string, map[string]any) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if toolsCalled.Load() != 0 {
		t.Fatal("unconfigured host tools resolved")
	}
}
