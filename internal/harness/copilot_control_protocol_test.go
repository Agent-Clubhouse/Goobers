package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	copilot "github.com/github/copilot-sdk/go"
)

// Exercise the production SDK framing/event path, not a fake RunPrompt. The
// only substitute is the owned CLI process's protocol endpoint; no model is
// contacted. Native session IDs must agree for preflight and model dispatch.
func TestCopilotControlledProtocolCapturesFinalResponse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var sends atomic.Int32
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		done <- serveCopilotReadinessFixture(conn, &sends)
	}()
	process := &fakeProcessRunner{act: func(req ProcessRequest) error {
		if !strings.Contains(strings.Join(req.Command, " "), "--headless") || strings.Contains(strings.Join(req.Command, " "), "private model prompt") {
			return fmt.Errorf("unexpected process arguments")
		}
		_, err := fmt.Fprintf(req.StdoutCapture, "listening on port %d\n", listener.Addr().(*net.TCPAddr).Port)
		return err
	}}
	// A process runner must stay alive while the SDK owns its connection.
	base := &readinessProtocolProcess{process: process}
	req := RunRequest{Workspace: t.TempDir(), Tools: goobersIOAvailableToolNames(), GoobersIORegistered: true}
	req.Envelope = testEnvelope(req.Workspace)
	config, err := goobersIOAdditionalMCPConfigArg(req, "/test/goobers")
	if err != nil {
		t.Fatal(err)
	}
	runner := &copilotControlledRunner{base: base, request: req, promptIndex: 1, mcpConfig: config}
	capture := newTranscriptBuffer(8192)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := runner.Run(ctx, ProcessRequest{Command: []string{"copilot", "-p=private model prompt", "--session-id", "owned-test-session", "--allow-all-tools"}, Dir: req.Workspace, Env: baseEnv(nil, nil), StdoutCapture: capture, Timeout: 2 * time.Second})
	runner.close()
	if err != nil {
		t.Fatal(err)
	}
	if sends.Load() != 1 || string(capture.Bytes()) != "final answer" || !strings.Contains(string(result.Transcript), "assistant.message") || runner.readiness.Category != "ready" {
		t.Fatalf("sends=%d capture=%q transcript=%q readiness=%+v", sends.Load(), capture.Bytes(), result.Transcript, runner.readiness)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type readinessProtocolProcess struct{ process *fakeProcessRunner }

func (p *readinessProtocolProcess) Run(ctx context.Context, req ProcessRequest) (ProcessResult, error) {
	result, err := p.process.Run(ctx, req)
	if err != nil {
		return result, err
	}
	<-ctx.Done()
	return result, ctx.Err()
}

func serveCopilotReadinessFixture(conn net.Conn, sends *atomic.Int32) error {
	reader := bufio.NewReader(conn)
	probed := false
	for {
		data, err := readReadinessFrame(reader)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		if err := json.Unmarshal(data, &req); err != nil {
			return err
		}
		result := any(map[string]any{})
		switch req.Method {
		case "connect":
			result = map[string]any{"protocolVersion": copilot.GetSDKProtocolVersion()}
		case "session.create":
			if req.Params["sessionId"] != "owned-test-session" {
				return fmt.Errorf("session identity lost: %v", req.Params["sessionId"])
			}
			result = map[string]any{"sessionId": "owned-test-session"}
		case "session.tools.initializeAndValidate":
		case "session.mcp.list":
			result = map[string]any{"servers": []map[string]any{{"name": goobersIOServerName, "status": "connected"}}}
		case "session.mcp.listTools":
			tools := []map[string]string{}
			for _, name := range goobersIOTools {
				tools = append(tools, map[string]string{"name": name})
			}
			result = map[string]any{"tools": tools}
		case "session.tools.execute":
			if req.Params["name"] != goobersIOServerName+"-get_run_info" {
				return fmt.Errorf("wrong probe")
			}
			probed = true
			result = map[string]any{"resultType": "success", "textResultForLlm": "{}"}
		case "session.send":
			if !probed || req.Params["sessionId"] != "owned-test-session" {
				return fmt.Errorf("model preceded same-session authorization")
			}
			sends.Add(1)
			result = map[string]any{"messageId": "message"}
		default:
			return fmt.Errorf("unexpected RPC %s", req.Method)
		}
		if err := writeReadinessFrame(conn, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}); err != nil {
			return err
		}
		if req.Method == "session.send" {
			for _, event := range []map[string]any{{"type": "assistant.message", "data": map[string]any{"messageId": "message", "content": "final answer"}}, {"type": "session.idle", "data": map[string]any{}}} {
				event["id"] = "fixture-event"
				event["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
				if err := writeReadinessFrame(conn, map[string]any{"jsonrpc": "2.0", "method": "session.event", "params": map[string]any{"sessionId": "owned-test-session", "event": event}}); err != nil {
					return err
				}
			}
		}
	}
}
func readReadinessFrame(reader *bufio.Reader) ([]byte, error) {
	length := 0
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if line == "\r\n" {
			break
		}
		if strings.HasPrefix(line, "Content-Length:") {
			length, err = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Content-Length:")))
			if err != nil {
				return nil, err
			}
		}
	}
	if length <= 0 || length > 1<<20 {
		return nil, fmt.Errorf("invalid frame length %d", length)
	}
	data := make([]byte, length)
	_, err := io.ReadFull(reader, data)
	return data, err
}
func writeReadinessFrame(writer io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(writer, "Content-Length: %d\r\n\r\n%s", len(data), data)
	return err
}
