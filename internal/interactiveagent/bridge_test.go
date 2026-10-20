//go:build !windows

package interactiveagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testTools() []Tool {
	return []Tool{{Name: "read_context", Description: "Read host-bound context.", InputSchema: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"text": map[string]any{"type": "string"}, "count": map[string]any{"type": "integer", "minimum": 0, "maximum": 4}}, "required": []string{"text"}}}}
}
func startTest(t *testing.T, options Options) *Bridge {
	t.Helper()
	if options.Tools == nil {
		options.Tools = testTools()
	}
	if options.Check == nil {
		options.Check = func(context.Context) error { return nil }
	}
	if options.Invoke == nil {
		options.Invoke = func(context.Context, Call) (any, error) { return map[string]any{"ok": true}, nil }
	}
	b, err := Start(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

type client struct {
	conn   net.Conn
	reader *bufio.Reader
	t      *testing.T
}

func newClient(t *testing.T, b *Bridge) *client {
	t.Helper()
	conn, err := net.Dial("unix", b.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	token, err := readToken(b.TokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = authenticateClient(conn, token); err != nil {
		t.Fatal(err)
	}
	c := &client{conn: conn, reader: bufio.NewReader(conn), t: t}
	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"test","version":"1"},"capabilities":{}}}`)
	if c.read().Error != nil {
		t.Fatal("initialize failed")
	}
	return c
}
func (c *client) send(raw string) {
	c.t.Helper()
	_ = c.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(c.conn, raw+"\n"); err != nil {
		c.t.Fatal(err)
	}
}
func (c *client) read() rpcResponse {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := c.reader.ReadBytes('\n')
	if err != nil {
		c.t.Fatal(err)
	}
	var response rpcResponse
	if json.Unmarshal(line, &response) != nil {
		c.t.Fatalf("bad response: %q", line)
	}
	return response
}
func (c *client) call(id string, args string) rpcResponse {
	c.send(`{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call","params":{"name":"read_context","arguments":` + args + `}}`)
	return c.read()
}
func responseText(response rpcResponse) string { raw, _ := json.Marshal(response); return string(raw) }
func requireToolError(t *testing.T, response rpcResponse, code string) {
	t.Helper()
	text := responseText(response)
	if !strings.Contains(text, `"isError":true`) || !strings.Contains(text, code) {
		t.Fatalf("expected tool error %s: %s", code, text)
	}
}

func TestUnixSocketStdioRoundTripAndPermissions(t *testing.T) {
	var calls atomic.Int32
	b := startTest(t, Options{Invoke: func(_ context.Context, call Call) (any, error) {
		calls.Add(1)
		return map[string]any{"received": call.Arguments}, nil
	}})
	if len(b.SocketPath) > 90 || !strings.HasPrefix(b.SocketPath, "/tmp/") {
		t.Fatal("socket path is not short/private")
	}
	for path, want := range map[string]os.FileMode{b.SocketPath: 0600, filepath.Dir(b.SocketPath): 0700} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("mode %s: %v %v", path, info, err)
		}
	}
	input := strings.Join([]string{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2099-01-01"}}`, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read_context","arguments":{"text":"hello","count":2}}}`, `{"jsonrpc":"2.0","id":4,"method":"ping"}`, `{"jsonrpc":"2.0","id":5,"method":"version"}`}, "\n") + "\n"
	var output bytes.Buffer
	if err := ServeStdio(context.Background(), b.SocketPath, b.TokenPath, strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 5 || calls.Load() != 1 {
		t.Fatalf("responses=%d calls=%d: %s", len(lines), calls.Load(), output.String())
	}
	if !strings.Contains(output.String(), `2025-11-25`) || !strings.Contains(output.String(), `read_context`) || !strings.Contains(output.String(), `hello`) {
		t.Fatal(output.String())
	}
	if names := b.ToolNames(); len(names) != 1 || names[0] != "read_context" {
		t.Fatal(names)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(b.SocketPath)); !os.IsNotExist(err) {
		t.Fatal("private socket directory survived close")
	}
}

func TestExactArgumentsAndReplayRemainHostOwned(t *testing.T) {
	var mu sync.Mutex
	var calls []Call
	b := startTest(t, Options{Invoke: func(_ context.Context, call Call) (any, error) {
		mu.Lock()
		calls = append(calls, call)
		mu.Unlock()
		return "ok", nil
	}})
	c := newClient(t, b)
	args := `{"text" : "unchanged", "count":2}`
	for i := 0; i < 2; i++ {
		response := c.call(`"stable-id"`, args)
		if strings.Contains(responseText(response), `"isError":true`) {
			t.Fatal(response)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatal("transport silently deduplicated or replayed host calls")
	}
	for _, call := range calls {
		if string(call.Arguments) != args || string(call.ID) != `"stable-id"` || call.Name != "read_context" {
			t.Fatalf("intent changed: %+v", call)
		}
	}
}

func TestUnknownToolsFieldsTypesAndIdentityAreDenied(t *testing.T) {
	var calls atomic.Int32
	b := startTest(t, Options{Invoke: func(context.Context, Call) (any, error) { calls.Add(1); return nil, nil }})
	c := newClient(t, b)
	for _, args := range []string{`{"text":"x","unknown":1}`, `{"text":"x","owner":"other"}`, `{"text":"x","sessionId":"other"}`, `{"text":"x","count":"2"}`, `{"text":"x","count":8}`, `{"text":"first","text":"second"}`, `null`, `[]`} {
		response := c.call(`2`, args)
		if response.Error == nil {
			requireToolError(t, response, "invalid_arguments")
		}
	}
	for _, raw := range []string{`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"approve_action","arguments":{}}}`, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"unknown","arguments":{}}}`} {
		c.send(raw)
		requireToolError(t, c.read(), "unknown_tool")
	}
	for _, raw := range []string{`{"jsonrpc":"2.0","id":2,"owner":"other","method":"tools/list"}`, `{"jsonrpc":"2.0","id":null,"method":"tools/list"}`, `{"jsonrpc":"2.0","id":{},"method":"tools/list"}`} {
		c.send(raw)
		if c.read().Error == nil {
			t.Fatal("invalid protocol request accepted")
		}
	}
	c.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read_context","arguments":{"text":"x"},"sessionId":"other"}}`)
	requireToolError(t, c.read(), "invalid_arguments")
	c.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read_context","arguments":{"text":"x"},"_meta":{"owner":"other"}}}`)
	requireToolError(t, c.read(), "invalid_arguments")
	if calls.Load() != 0 {
		t.Fatal("invalid input reached host")
	}
}

func TestNotificationsCannotInvokeTools(t *testing.T) {
	var calls atomic.Int32
	b := startTest(t, Options{Invoke: func(context.Context, Call) (any, error) { calls.Add(1); return nil, nil }})
	c := newClient(t, b)
	c.send(`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"read_context","arguments":{"text":"x"}}}`)
	c.send(`{"jsonrpc":"2.0","id":3,"method":"ping"}`)
	if c.read().Error != nil {
		t.Fatal("ping failed")
	}
	if calls.Load() != 0 {
		t.Fatal("notification launched an untracked tool")
	}
}

func TestStaleLeaseDenyHasNoInternalErrorOrArguments(t *testing.T) {
	var calls atomic.Int32
	var events []AuditEvent
	var mu sync.Mutex
	b := startTest(t, Options{Check: func(context.Context) error {
		return errors.New("private lease path and Bearer " + strings.Repeat("a", 32))
	}, Invoke: func(context.Context, Call) (any, error) { calls.Add(1); return nil, nil }, Emit: func(event AuditEvent) { mu.Lock(); events = append(events, event); mu.Unlock() }})
	c := newClient(t, b)
	response := c.call(`2`, `{"text":"private user prompt"}`)
	requireToolError(t, response, "execution_unavailable")
	if text := responseText(response); strings.Contains(text, "private") || strings.Contains(text, "Bearer") {
		t.Fatal("private diagnostic leaked")
	}
	if calls.Load() != 0 {
		t.Fatal("stale lease invoked")
	}
	mu.Lock()
	defer mu.Unlock()
	raw, _ := json.Marshal(events)
	if strings.Contains(string(raw), "private") || len(events) != 1 || events[0].Phase != "denied" {
		t.Fatal(string(raw))
	}
}

func TestToolInvocationSerializesAcrossSockets(t *testing.T) {
	var active, maxActive atomic.Int32
	entered := make(chan struct{}, 2)
	release := make(chan struct{}, 2)
	b := startTest(t, Options{Invoke: func(ctx context.Context, _ Call) (any, error) {
		now := active.Add(1)
		for {
			old := maxActive.Load()
			if now <= old || maxActive.CompareAndSwap(old, now) {
				break
			}
		}
		defer active.Add(-1)
		entered <- struct{}{}
		select {
		case <-release:
			return "done", nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}})
	a := newClient(t, b)
	c := newClient(t, b)
	a.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read_context","arguments":{"text":"first"}}}`)
	<-entered
	c.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read_context","arguments":{"text":"second"}}}`)
	release <- struct{}{}
	a.read()
	<-entered
	release <- struct{}{}
	c.read()
	if maxActive.Load() != 1 {
		t.Fatal("host callbacks overlapped")
	}
}

func TestRequestCancellationCancelsHostInvocation(t *testing.T) {
	entered := make(chan struct{})
	ended := make(chan struct{})
	b := startTest(t, Options{Invoke: func(ctx context.Context, _ Call) (any, error) {
		close(entered)
		<-ctx.Done()
		close(ended)
		return nil, ctx.Err()
	}})
	c := newClient(t, b)
	c.send(`{"jsonrpc":"2.0","id":"cancel-me","method":"tools/call","params":{"name":"read_context","arguments":{"text":"x"}}}`)
	<-entered
	c.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"cancel-me","reason":"user stopped"}}`)
	requireToolError(t, c.read(), "cancelled")
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("host context was not cancelled")
	}
}

func TestBridgeCancellationStopsInvocationAndRemovesSocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	ended := make(chan struct{})
	b, err := Start(ctx, Options{Tools: testTools(), Check: func(context.Context) error { return nil }, Invoke: func(ctx context.Context, _ Call) (any, error) {
		close(entered)
		<-ctx.Done()
		close(ended)
		return nil, ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := b.Close(); err != nil {
			t.Error(err)
		}
	}()
	c := newClient(t, b)
	c.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read_context","arguments":{"text":"x"}}}`)
	<-entered
	cancel()
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("invocation did not cancel")
	}
	if err = b.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = net.Dial("unix", b.SocketPath); err == nil {
		t.Fatal("cancelled bridge still accepts calls")
	}
}

func TestCancellationDuringCheckCannotDispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	b, err := Start(ctx, Options{Tools: testTools(), Check: func(context.Context) error { cancel(); return nil }, Invoke: func(context.Context, Call) (any, error) { calls.Add(1); return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := b.Close(); err != nil {
			t.Error(err)
		}
	}()
	c := newClient(t, b)
	c.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read_context","arguments":{"text":"x"}}}`)
	_ = c.conn.SetReadDeadline(time.Now().Add(time.Second))
	_, _ = c.reader.ReadBytes('\n')
	if calls.Load() != 0 {
		t.Fatal("cancelled check dispatched effect")
	}
}

func TestOversizedMessageIsRejectedBeforeHostDispatch(t *testing.T) {
	var calls atomic.Int32
	b := startTest(t, Options{Invoke: func(context.Context, Call) (any, error) { calls.Add(1); return nil, nil }})
	c := newClient(t, b)
	_, _ = io.WriteString(c.conn, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read_context","arguments":{"text":"`+strings.Repeat("x", MaxRequestBytes)+`"}}}`+"\n")
	response := c.read()
	if response.Error == nil || calls.Load() != 0 {
		t.Fatal("oversized request accepted")
	}
	var output bytes.Buffer
	if err := ServeStdio(context.Background(), b.SocketPath, b.TokenPath, strings.NewReader(strings.Repeat("x", MaxRequestBytes+1)), &output); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("proxy limit error: %v", err)
	}
}

func TestUnsafeOutputsAndCallbackPanicsAreSanitized(t *testing.T) {
	for _, kind := range []string{"error", "panic", "secret", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			b := startTest(t, Options{Invoke: func(context.Context, Call) (any, error) {
				switch kind {
				case "error":
					return nil, errors.New("sensitive internal detail")
				case "panic":
					panic("sensitive internal detail")
				case "secret":
					return "Bearer " + strings.Repeat("a", 32), nil
				default:
					return strings.Repeat("x", MaxResponseBytes), nil
				}
			}})
			c := newClient(t, b)
			response := c.call(`2`, `{"text":"x"}`)
			text := responseText(response)
			if !strings.Contains(text, `"isError":true`) || strings.Contains(text, "sensitive") || strings.Contains(text, "Bearer") {
				t.Fatal(text)
			}
		})
	}
}

func TestStdioCancellationClosesBlockedInput(t *testing.T) {
	b := startTest(t, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	reader, writer := io.Pipe()
	defer func() {
		if err := writer.Close(); err != nil {
			t.Error(err)
		}
	}()
	done := make(chan error, 1)
	go func() { done <- ServeStdio(ctx, b.SocketPath, b.TokenPath, reader, io.Discard) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrUnavailable) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("stdio did not stop on cancellation")
	}
}

func TestUnsafeSocketPermissionsAreRefused(t *testing.T) {
	b := startTest(t, Options{})
	if err := os.Chmod(b.SocketPath, 0666); err != nil {
		t.Fatal(err)
	}
	if err := ServeStdio(context.Background(), b.SocketPath, b.TokenPath, strings.NewReader(""), io.Discard); !errors.Is(err, ErrUnavailable) {
		t.Fatal("proxy accepted an unprotected socket")
	}
}

func TestConfigurationForbidsApprovalAndAuthoritySurface(t *testing.T) {
	for _, name := range []string{"approve_action", "grant_connection", "get_credentials", "reauthenticate", "authorize_write"} {
		tools := testTools()
		tools[0].Name = name
		if _, err := Start(context.Background(), Options{Tools: tools, Check: func(context.Context) error { return nil }, Invoke: func(context.Context, Call) (any, error) { return nil, nil }}); !errors.Is(err, ErrInvalidConfiguration) {
			t.Fatalf("exposed authority tool %s: %v", name, err)
		}
	}
	for _, field := range []string{"owner", "sessionId", "lease_id", "executionFence", "authorityRef", "access_token", "approval"} {
		tools := testTools()
		tools[0].InputSchema["properties"].(map[string]any)[field] = map[string]any{"type": "string"}
		if _, _, err := compileTools(tools); err == nil {
			t.Fatalf("authority field %s allowed", field)
		}
	}
	tools := testTools()
	tools[0].InputSchema["$ref"] = "https://untrusted.invalid/schema"
	if _, _, err := compileTools(tools); err == nil {
		t.Fatal("external schema load allowed")
	}
}

func TestSchemaAndToolSnapshotsCannotBeMutated(t *testing.T) {
	tools := testTools()
	b := startTest(t, Options{Tools: tools})
	tools[0].Name = "approve_action"
	tools[0].InputSchema["additionalProperties"] = true
	c := newClient(t, b)
	requireToolError(t, c.call(`2`, `{"text":"x","arbitrary":true}`), "invalid_arguments")
	c.send(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	text := responseText(c.read())
	if strings.Contains(text, "approve_action") || !strings.Contains(text, "read_context") {
		t.Fatal("configuration aliases retained tool definitions")
	}
}

func TestMalformedAndDeepJSONNeverReachesHost(t *testing.T) {
	var calls atomic.Int32
	b := startTest(t, Options{Invoke: func(context.Context, Call) (any, error) { calls.Add(1); return nil, nil }})
	c := newClient(t, b)
	for _, raw := range []string{`{"jsonrpc":"2.0",`, fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read_context","arguments":%s0%s}}`, strings.Repeat("[", 40), strings.Repeat("]", 40)), `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read_context","arguments":{"text":"Bearer aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}`} {
		c.send(raw)
		if c.read().Error == nil {
			t.Fatal("malformed/unsafe message accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("unsafe input invoked host")
	}
}
