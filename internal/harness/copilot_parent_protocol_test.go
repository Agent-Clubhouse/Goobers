package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"

	"github.com/goobers/goobers/internal/apicontract/childworkflowwire"
	"github.com/goobers/goobers/internal/mcpio"
)

// Only the daemon's HTTP responses and the model are substituted. The SDK
// session registration, permission events, private MCP configuration, source
// loading, tool argument validation and scoped HTTP requests run normally.
func copilotParentDaemonFixture(t *testing.T) string {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		paths = append(paths, r.URL.Path)
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer goobers-child.parent-test" {
			t.Error("child request lost launcher authority")
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case strings.ReplaceAll(childworkflowwire.ValidatePath, "{run}", "run-1"):
			if !strings.Contains(body["source"], "dslVersion: \"3.1\"") {
				t.Error("validation lost authored DSL")
			}
			_, _ = io.WriteString(w, `{"valid":true}`)
		case strings.ReplaceAll(childworkflowwire.StartPath, "{run}", "run-1"):
			if r.Header.Get("Idempotency-Key") != "parent-child" || body["source"] == "" {
				t.Error("child start lost source or stable identity")
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"state":"queued"}`)
		case strings.ReplaceAll(childworkflowwire.StatusPath, "{run}", "run-1"):
			if body["invocationKey"] != "parent-child" {
				t.Error("child status identity changed")
			}
			_, _ = fmt.Fprintf(w, `{"state":"completed","resultRef":%q}`, "sha256:"+strings.Repeat("a", 64))
		case strings.ReplaceAll(childworkflowwire.ResolvePath, "{run}", "run-1"):
			if body["invocationKey"] != "parent-child" || body["action"] != "merge" || body["resultRef"] != "sha256:"+strings.Repeat("a", 64) {
				t.Error("child resolution lost exact result or choice")
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"applied":false}`)
		default:
			t.Errorf("unexpected child path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(func() {
		server.Close()
		mu.Lock()
		defer mu.Unlock()
		expected := []string{childworkflowwire.ValidatePath, childworkflowwire.StartPath, childworkflowwire.StatusPath, childworkflowwire.ResolvePath}
		if len(paths) != len(expected) {
			t.Errorf("child HTTP calls=%v", paths)
			return
		}
		for i, path := range expected {
			if paths[i] != strings.ReplaceAll(path, "{run}", "run-1") {
				t.Errorf("child request order=%v", paths)
			}
		}
	})
	return server.URL
}

func exerciseCopilotParentMCP(configPath string) error {
	cfg, err := mcpio.LoadConfig(configPath)
	if err != nil {
		return err
	}
	source := "apiVersion: goobers.dev/v1alpha1\nkind: Workflow\ndslVersion: \"3.1\"\nmetadata: {name: generated-child}\nspec:\n  gaggle: example\n  triggers: [{type: manual}]\n  start: check\n  tasks:\n    - name: check\n      type: deterministic\n      goal: Check the generated workstream\n      run: {command: [echo, child], workspace: scratch}\n"
	if err := os.WriteFile(filepath.Join(cfg.Workspace, "child.yaml"), []byte(source), 0600); err != nil {
		return err
	}
	var input, output bytes.Buffer
	encode := json.NewEncoder(&input)
	requests := []map[string]any{
		{"method": "initialize", "params": map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "copilot-parent-fixture", "version": "1"}}},
		{"method": "tools/call", "params": map[string]any{"name": "validate_child_workflow", "arguments": map[string]string{"sourceFile": "child.yaml"}}},
		{"method": "tools/call", "params": map[string]any{"name": "start_child_workflow", "arguments": map[string]string{"sourceFile": "child.yaml", "invocationKey": "parent-child"}}},
		{"method": "tools/call", "params": map[string]any{"name": "get_child_workflow", "arguments": map[string]string{"invocationKey": "parent-child"}}},
		{"method": "tools/call", "params": map[string]any{"name": "resolve_child_workflow", "arguments": map[string]string{"invocationKey": "parent-child", "action": "merge", "resultRef": "sha256:" + strings.Repeat("a", 64)}}},
	}
	for i, request := range requests {
		request["jsonrpc"] = "2.0"
		request["id"] = i + 1
		if err := encode.Encode(request); err != nil {
			return err
		}
	}
	server := mcpio.NewServer(mcpio.NewToolset(cfg))
	if err := server.Serve(&input, &output, io.Discard); err != nil {
		return err
	}
	decoder := json.NewDecoder(&output)
	for range requests {
		var response struct {
			Error  json.RawMessage `json:"error"`
			Result struct {
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		if err := decoder.Decode(&response); err != nil {
			return err
		}
		if len(response.Error) > 0 || response.Result.IsError {
			return fmt.Errorf("parent MCP tool refused")
		}
	}
	return nil
}

func copilotParentSessionConfig(params map[string]any) (string, error) {
	encoded, err := json.Marshal(params)
	if err != nil {
		return "", err
	}
	var registration struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
			Tools   []string `json:"tools"`
		} `json:"mcpServers"`
		AvailableTools []string `json:"availableTools"`
	}
	if err := json.Unmarshal(encoded, &registration); err != nil {
		return "", err
	}
	server := registration.MCPServers[goobersIOServerName]
	if server.Command != "/test/goobers" || len(server.Args) != 3 || server.Args[0] != "mcp-io" {
		return "", fmt.Errorf("parent MCP launch registration lost")
	}
	for _, tool := range []string{"validate_child_workflow", "start_child_workflow", "get_child_workflow", "resolve_child_workflow"} {
		if !slices.Contains(server.Tools, tool) || !slices.Contains(registration.AvailableTools, goobersIOServerName+"-"+tool) {
			return "", fmt.Errorf("parent SDK registration lost %s", tool)
		}
	}
	cfg, err := mcpio.LoadConfig(server.Args[2])
	if err != nil {
		return "", err
	}
	if cfg.ChildWorkflows == nil || strings.Contains(string(encoded), cfg.ChildWorkflows.BearerToken) || strings.Contains(string(encoded), cfg.ChildWorkflows.Endpoint) {
		return "", fmt.Errorf("parent grant missing or exposed in SDK session")
	}
	return server.Args[2], nil
}

func TestCopilotParentSDKPermissionsRequireLauncherGrant(t *testing.T) {
	req := childHarnessRequest(t)
	tools := mcpio.ChildWorkflowToolNames(req.ChildWorkflows, req.Envelope.RunID)
	for _, parent := range []bool{true, false} {
		scoped := req
		if !parent {
			scoped.ChildWorkflows = nil
		}
		scoped = withAutoGoobersIO(scoped, "/test/goobers")
		runner := &copilotControlledRunner{request: scoped}
		handler := runner.sessionConfig("session", ProcessRequest{Dir: req.Workspace}, nil).OnPermissionRequest
		for _, tool := range tools {
			decision, err := handler(rpc.PermissionRequestMCP{ServerName: goobersIOServerName, ToolName: tool}, copilot.PermissionInvocation{})
			_, approved := decision.(*rpc.PermissionDecisionApproveOnce)
			if err != nil || approved != parent {
				t.Fatalf("parent=%v tool=%s decision=%T err=%v", parent, tool, decision, err)
			}
		}
	}
}
