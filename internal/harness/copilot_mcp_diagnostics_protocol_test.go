package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	copilot "github.com/github/copilot-sdk/go"
)

// The only fake boundary is the owned CLI transport. The production SDK,
// session initialization, native tool execution and cleanup are exercised.
func TestMCPDiagnosticsNativeProtocolNeverSendsModelPrompt(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	done := make(chan error, 1)
	var probes atomic.Int32
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = conn.Close() }()
		done <- serveMCPDiagnosticFixture(conn, &probes)
	}()
	process := &readinessProtocolProcess{process: &fakeProcessRunner{act: func(req ProcessRequest) error {
		for _, arg := range req.Command {
			if strings.HasPrefix(arg, "-p=") {
				return errors.New("model prompt reached subprocess")
			}
		}
		_, err := fmt.Fprintf(req.StdoutCapture, "listening on port %d\n", listener.Addr().(*net.TCPAddr).Port)
		return err
	}}}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, SelfBin: "/trusted/goobers", Runner: process,
		mcpSessionFactory: func(ctx context.Context, req ProcessRequest, runner *copilotControlledRunner) (copilotModelSession, error) {
			if err := runner.initialize(ctx, req); err != nil {
				return nil, err
			}
			return runner.session, nil
		}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	reports := adapter.ProbeMCPReadiness(ctx, RunRequest{})
	if len(reports) != 1 || reports[0].Category != "ready" || probes.Load() != 1 {
		t.Fatalf("reports=%+v probes=%d", reports, probes.Load())
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func serveMCPDiagnosticFixture(conn net.Conn, probes *atomic.Int32) error {
	reader := bufio.NewReader(conn)
	for {
		data, err := readReadinessFrame(reader)
		if errors.Is(err, io.EOF) {
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
			result = map[string]any{"sessionId": req.Params["sessionId"]}
		case "session.tools.initializeAndValidate":
		case "session.mcp.list":
			result = map[string]any{"servers": []map[string]any{{"name": goobersIOServerName, "status": "connected"}}}
		case "session.mcp.listTools":
			var tools []map[string]string
			for _, name := range goobersIOTools {
				tools = append(tools, map[string]string{"name": name})
			}
			result = map[string]any{"tools": tools}
		case "session.tools.execute":
			if req.Params["name"] != goobersIOServerName+"-get_run_info" {
				return fmt.Errorf("mutation-capable tool execution: %v", req.Params["name"])
			}
			probes.Add(1)
			result = map[string]any{"resultType": "success", "textResultForLlm": "private-response-must-not-be-reported"}
		default:
			return fmt.Errorf("unexpected or model RPC: %s", req.Method)
		}
		if err := writeReadinessFrame(conn, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}); err != nil {
			return err
		}
	}
}
