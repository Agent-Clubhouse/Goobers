//go:build integration && !windows

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

// This read-only live probe creates no model turn: a missing required server
// must stop the real controlled session before SendAndWait is reachable.
func TestIntegrationCopilotRequiredMCPAbsentBeforeModel(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_COPILOT_MCP_READINESS")
	testdep.Require(t, "copilot")
	workspace := t.TempDir()
	var report MCPReadiness
	adapter := &CopilotAdapter{Command: []string{"copilot"}, SelfBin: "/nonexistent/goobers-required-mcp-readiness"}
	// Carry the preflight version as the runtime does, so a CLI new enough for
	// usage-file capture puts --usage-output-file in the stage argv and the
	// real control process proves it accepts what remains (#5636).
	info, err := adapter.Preflight(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Run(context.Background(), RunRequest{
		Envelope: testEnvelope(workspace), Workspace: workspace, CompletionPath: DefaultResultPath, HarnessConfigResolved: true, HarnessVersion: info.Version,
		Timeout: 30 * time.Second, MCPReadinessSink: func(r MCPReadiness) error { report = r; return nil },
	})
	if !errors.Is(err, errRequiredMCPUnavailable) {
		t.Fatalf("missing required server error=%v", err)
	}
	if report.Category != "required_tool_unavailable" {
		t.Fatalf("did not reach actual MCP observation: %+v error=%v", report, err)
	}
}

func TestIntegrationCopilotRequiredMCPReadOnlyAuthorization(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_COPILOT_MCP_READINESS")
	testdep.Require(t, "copilot")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if len(request.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "readiness-fixture", "version": "1"}}
		case "tools/list":
			var tools []map[string]any
			for _, name := range goobersIOTools {
				tools = append(tools, map[string]any{"name": name, "description": "readiness fixture", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}})
			}
			result = map[string]any{"tools": tools}
		case "tools/call":
			if request.Params.Name != "get_run_info" {
				t.Errorf("unexpected tool mutation: %s", request.Params.Name)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			calls.Add(1)
			result = map[string]any{"content": []map[string]string{{"type": "text", "text": `{"runID":"readiness"}`}}}
		default:
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	defer server.Close()
	workspace := t.TempDir()
	config := filepath.Join(workspace, "mcp.json")
	data, _ := json.Marshal(copilotMCPConfig{MCPServers: map[string]copilotMCPServer{goobersIOServerName: {Type: "http", URL: server.URL, Tools: goobersIOTools}}})
	if err := os.WriteFile(config, data, 0600); err != nil {
		t.Fatal(err)
	}
	sessionID, err := newHarnessSessionID()
	if err != nil {
		t.Fatal(err)
	}
	runner := &copilotControlledRunner{base: ExecProcessRunner{}, request: RunRequest{Workspace: workspace, Tools: goobersIOAvailableToolNames()}, promptIndex: 1, mcpConfig: config}
	defer runner.close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := runner.initialize(ctx, ProcessRequest{Command: []string{"copilot", "-p=must not run", "--session-id", sessionID, "--allow-all-tools", "--log-dir", filepath.Join(workspace, "logs")}, Dir: workspace, Env: overrideEnv(baseEnv(nil, nil), "COPILOT_HOME", filepath.Join(workspace, "copilot-home")), Timeout: 30 * time.Second}); err != nil {
		t.Fatal(err)
	}
	report, err := probeRequiredMCPSession(ctx, runner.session)
	if err != nil || report.Connection != "ready" || report.Inventory != "ready" {
		t.Fatalf("report=%+v calls=%d err=%v", report, calls.Load(), err)
	}
	if failures := copilotRunnerMCPFailures(ctx, runner, RunRequest{GoobersIORegistered: true}, filepath.Join(workspace, "logs")); len(failures) != 0 {
		t.Fatalf("post-run evidence disagrees: %+v", failures)
	}
	runner.ready = true
	if err := finalizeControlledCopilot(ctx, runner); err != nil {
		t.Fatalf("real runtime usage/finalization: %v", err)
	}
	if report.Category == "check_unobservable" && report.Authorization == "unobservable" && calls.Load() == 0 {
		t.Log("native authorization RPC unavailable; connection and inventory verified without a model turn")
		return
	}
	if report.Category != "ready" || report.Authorization != "ready" || calls.Load() != 1 {
		t.Fatalf("report=%+v calls=%d err=%v", report, calls.Load(), err)
	}
}
