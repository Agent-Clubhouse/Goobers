package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

const (
	mcpClientHelperEnv = "GOOBERS_MCP_CLIENT_HELPER"
	stdioMCPMarkerArg  = "goobers-mcp-stdio"
	stdioMCPTokenEnv   = "STDIO_MCP_TOKEN"
	stdioMCPSecret     = "opaque-stdio-mcp-secret"
	remoteMCPSecret    = "opaque-remote-mcp-secret"
)

type testCopilotMCPConfig struct {
	MCPServers map[string]testCopilotMCPServer `json:"mcpServers"`
}

type testCopilotConfig struct {
	OAuthToken       string                       `json:"oauthToken"`
	InstalledPlugins []testCopilotInstalledPlugin `json:"installedPlugins"`
}

type testCopilotInstalledPlugin struct {
	Name      string `json:"name"`
	CachePath string `json:"cache_path"`
}

type testCopilotMCPServer struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	URL     string            `json:"url"`
	Tools   []string          `json:"tools"`
	Env     map[string]string `json:"env"`
	Headers map[string]string `json:"headers"`
}

type testMCPRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type testMCPResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Result  struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func TestCopilotAdapterReachesOnlyInvocationScopedMCPServers(t *testing.T) {
	var declaredRemoteCalls atomic.Int32
	declaredRemote := newTestRemoteMCPServer(t, "declared-remote-reachable", "Bearer "+remoteMCPSecret, &declaredRemoteCalls)
	defer declaredRemote.Close()

	var ambientRemoteCalls atomic.Int32
	ambientRemote := newTestRemoteMCPServer(t, "ambient-remote-reachable", "", &ambientRemoteCalls)
	defer ambientRemote.Close()

	var ambientPluginCalls atomic.Int32
	ambientPlugin := newTestRemoteMCPServer(t, "ambient-plugin-reachable", "", &ambientPluginCalls)
	defer ambientPlugin.Close()

	ambientHome := filepath.Join(t.TempDir(), "ambient-copilot")
	if err := os.MkdirAll(ambientHome, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestCopilotMCPConfig(t, ambientHome, map[string]testCopilotMCPServer{
		"ambient": {
			Type:  "http",
			URL:   ambientRemote.URL,
			Tools: []string{"*"},
		},
	})
	pluginCache := t.TempDir()
	writeTestCopilotMCPConfigFile(t, filepath.Join(pluginCache, ".mcp.json"), map[string]testCopilotMCPServer{
		"ambient-plugin": {
			Type:  "http",
			URL:   ambientPlugin.URL,
			Tools: []string{"*"},
		},
	})
	writeTestCopilotConfig(t, ambientHome, []testCopilotInstalledPlugin{{
		Name:      "ambient-tools",
		CachePath: pluginCache,
	}})
	t.Setenv("COPILOT_HOME", ambientHome)
	t.Setenv(copilotPluginDirOnlyEnv, "false")
	t.Setenv(mcpClientHelperEnv, "1")

	adapter := &CopilotAdapter{
		Command:           []string{os.Args[0], "-test.run=^TestCopilotMCPClientHelper$"},
		PromptFlag:        "-test.paniconexit0",
		ExtraArgs:         []string{"--resume"},
		Runner:            ExecProcessRunner{},
		ExtraEnvAllowlist: []string{"COPILOT_HOME", copilotPluginDirOnlyEnv, mcpClientHelperEnv},
	}
	stdioMarker := filepath.Join(t.TempDir(), "stdio-calls")
	workspace := t.TempDir()
	first, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace, "contents:read", "github:issues:write"),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Timeout:        30 * time.Second,
		Credentials: twoTokenCredentials(
			t,
			"contents:read", stdioMCPSecret,
			"github:issues:write", remoteMCPSecret,
		),
		MCPServers: []apiv1.MCPServer{
			{
				Name:    "declared-stdio",
				Command: os.Args[0],
				Args: []string{
					"-test.run=^TestCopilotMCPStdioServerHelper$",
					stdioMCPMarkerArg,
					stdioMarker,
				},
				CredentialRefs: []apiv1.MCPCredentialRef{{
					Capability: "contents:read",
					Env:        stdioMCPTokenEnv,
				}},
			},
			{
				Name: "declared-remote",
				URL:  declaredRemote.URL,
				CredentialRefs: []apiv1.MCPCredentialRef{{
					Capability: "github:issues:write",
					Header:     "Authorization",
					Scheme:     apiv1.MCPHeaderSchemeBearer,
				}},
			},
		},
	})
	if err != nil {
		t.Fatalf("scoped MCP invocation: %v", err)
	}
	assertMCPResults(t, first.Payload, map[string]string{
		"declared-remote": "declared-remote-reachable",
		"declared-stdio":  "declared-stdio-reachable",
	})
	if got := declaredRemoteCalls.Load(); got != 1 {
		t.Fatalf("declared remote tool calls = %d, want 1", got)
	}
	if got := ambientRemoteCalls.Load(); got != 0 {
		t.Fatalf("ambient remote tool calls during scoped invocation = %d, want 0", got)
	}
	if got := ambientPluginCalls.Load(); got != 0 {
		t.Fatalf("ambient plugin tool calls during scoped invocation = %d, want 0", got)
	}
	assertStdioMCPCalls(t, stdioMarker, 1)

	workspace = t.TempDir()
	second, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Timeout:        30 * time.Second,
	})
	if err != nil {
		t.Fatalf("MCP-free invocation: %v", err)
	}
	assertMCPResults(t, second.Payload, map[string]string{
		"ambient":        "ambient-remote-reachable",
		"ambient-plugin": "ambient-plugin-reachable",
	})
	if got := declaredRemoteCalls.Load(); got != 1 {
		t.Fatalf("declared remote tool calls after MCP-free invocation = %d, want 1", got)
	}
	if got := ambientRemoteCalls.Load(); got != 1 {
		t.Fatalf("ambient remote tool calls after MCP-free invocation = %d, want 1", got)
	}
	if got := ambientPluginCalls.Load(); got != 1 {
		t.Fatalf("ambient plugin tool calls after MCP-free invocation = %d, want 1", got)
	}
	assertStdioMCPCalls(t, stdioMarker, 1)
}

func TestCopilotMCPClientHelper(t *testing.T) {
	if os.Getenv(mcpClientHelperEnv) != "1" {
		return
	}
	home := os.Getenv("COPILOT_HOME")
	raw, err := os.ReadFile(filepath.Join(home, "mcp-config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config testCopilotMCPConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	scoped := strings.Contains(filepath.ToSlash(home), "/.goobers/mcp/runtime-")
	if scoped != slices.Contains(os.Args, "--disable-builtin-mcps") {
		t.Fatalf("scoped home = %v, --disable-builtin-mcps args = %v", scoped, os.Args)
	}
	pluginDirOnly := strings.EqualFold(os.Getenv(copilotPluginDirOnlyEnv), "true")
	if scoped != pluginDirOnly {
		t.Fatalf("scoped home = %v, %s = %q", scoped, copilotPluginDirOnlyEnv, os.Getenv(copilotPluginDirOnlyEnv))
	}
	plugins, err := readTestCopilotInstalledPlugins(home)
	if err != nil {
		t.Fatal(err)
	}
	if scoped && len(plugins) != 0 {
		t.Fatalf("scoped invocation inherited ambient plugins: %#v", plugins)
	}
	if !pluginDirOnly {
		if config.MCPServers == nil {
			config.MCPServers = make(map[string]testCopilotMCPServer)
		}
		for _, plugin := range plugins {
			servers, err := readTestCopilotMCPConfig(filepath.Join(plugin.CachePath, ".mcp.json"))
			if err != nil {
				t.Fatalf("load plugin %q MCP config: %v", plugin.Name, err)
			}
			for name, server := range servers {
				if _, exists := config.MCPServers[name]; exists {
					t.Fatalf("duplicate MCP server %q", name)
				}
				config.MCPServers[name] = server
			}
		}
	}

	names := make([]string, 0, len(config.MCPServers))
	for name := range config.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)
	outputs := make(map[string]interface{}, len(names))
	for _, name := range names {
		result, err := callTestMCPServer(config.MCPServers[name])
		if err != nil {
			t.Fatalf("call MCP server %q: %v", name, err)
		}
		outputs[name] = result
	}
	if err := WriteCompletion(".", DefaultResultPath, apiv1.ResultEnvelope{
		Status:  apiv1.ResultSuccess,
		Outputs: outputs,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCopilotMCPStdioServerHelper(t *testing.T) {
	markerIndex := slices.Index(os.Args, stdioMCPMarkerArg)
	if markerIndex < 0 {
		return
	}
	if markerIndex+1 >= len(os.Args) {
		t.Fatal("stdio MCP marker path missing")
	}
	if got := os.Getenv(stdioMCPTokenEnv); got != stdioMCPSecret {
		t.Fatalf("%s = %q, want scoped credential", stdioMCPTokenEnv, got)
	}

	decoder := json.NewDecoder(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for {
		var request testMCPRequest
		if err := decoder.Decode(&request); err != nil {
			t.Fatal(err)
		}
		switch request.Method {
		case "initialize":
			if err := encoder.Encode(testMCPInitializeResponse(request.ID)); err != nil {
				t.Fatal(err)
			}
		case "tools/call":
			if !isReachabilityToolCall(request) {
				t.Fatalf("unexpected MCP tool call: %#v", request.Params)
			}
			marker, err := os.OpenFile(os.Args[markerIndex+1], os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := marker.WriteString("called\n"); err != nil {
				_ = marker.Close()
				t.Fatal(err)
			}
			if err := marker.Close(); err != nil {
				t.Fatal(err)
			}
			if err := encoder.Encode(testMCPToolResponse(request.ID, "declared-stdio-reachable")); err != nil {
				t.Fatal(err)
			}
			return
		default:
			t.Fatalf("unexpected MCP method %q", request.Method)
		}
	}
}

func callTestMCPServer(server testCopilotMCPServer) (string, error) {
	if !slices.Equal(server.Tools, []string{"*"}) {
		return "", fmt.Errorf("tools = %v, want [*]", server.Tools)
	}
	switch server.Type {
	case "local":
		if server.Command == "" || server.URL != "" {
			return "", fmt.Errorf("invalid local MCP config: %#v", server)
		}
		return callTestStdioMCPServer(server)
	case "http":
		if server.URL == "" || server.Command != "" {
			return "", fmt.Errorf("invalid HTTP MCP config: %#v", server)
		}
		return callTestRemoteMCPServer(server)
	default:
		return "", fmt.Errorf("unsupported MCP type %q", server.Type)
	}
}

func callTestStdioMCPServer(server testCopilotMCPServer) (string, error) {
	command := exec.Command(server.Command, server.Args...)
	command.Env = append([]string(nil), os.Environ()...)
	for name, value := range server.Env {
		command.Env = append(command.Env, name+"="+os.ExpandEnv(value))
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		return "", err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return "", err
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return "", err
	}
	encoder := json.NewEncoder(stdin)
	decoder := json.NewDecoder(stdout)
	if _, err := testMCPRoundTrip(encoder, decoder, testMCPInitializeRequest(1)); err != nil {
		return "", finishTestMCPProcess(command, stdin, stderr.String(), err)
	}
	response, err := testMCPRoundTrip(encoder, decoder, testMCPToolRequest(2))
	closeErr := stdin.Close()
	waitErr := command.Wait()
	if err != nil {
		return "", fmt.Errorf("call tool: %w", err)
	}
	if closeErr != nil {
		return "", closeErr
	}
	if waitErr != nil {
		return "", fmt.Errorf("%w: %s", waitErr, stderr.String())
	}
	return testMCPToolText(response)
}

func finishTestMCPProcess(command *exec.Cmd, stdin interface{ Close() error }, stderr string, cause error) error {
	_ = stdin.Close()
	waitErr := command.Wait()
	if waitErr != nil {
		return fmt.Errorf("%w; process: %w: %s", cause, waitErr, stderr)
	}
	return cause
}

func callTestRemoteMCPServer(server testCopilotMCPServer) (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	headers := make(http.Header, len(server.Headers)+2)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json, text/event-stream")
	for name, value := range server.Headers {
		headers.Set(name, os.ExpandEnv(value))
	}
	if _, err := testMCPHTTPRoundTrip(client, server.URL, headers, testMCPInitializeRequest(1)); err != nil {
		return "", fmt.Errorf("initialize: %w", err)
	}
	response, err := testMCPHTTPRoundTrip(client, server.URL, headers, testMCPToolRequest(2))
	if err != nil {
		return "", fmt.Errorf("call tool: %w", err)
	}
	return testMCPToolText(response)
}

func testMCPRoundTrip(encoder *json.Encoder, decoder *json.Decoder, request testMCPRequest) (testMCPResponse, error) {
	if err := encoder.Encode(request); err != nil {
		return testMCPResponse{}, err
	}
	var response testMCPResponse
	if err := decoder.Decode(&response); err != nil {
		return testMCPResponse{}, err
	}
	if response.Error != nil {
		return testMCPResponse{}, fmt.Errorf("MCP error %d: %s", response.Error.Code, response.Error.Message)
	}
	return response, nil
}

func testMCPHTTPRoundTrip(client *http.Client, url string, headers http.Header, request testMCPRequest) (testMCPResponse, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return testMCPResponse{}, err
	}
	httpRequest, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return testMCPResponse{}, err
	}
	httpRequest.Header = headers.Clone()
	response, err := client.Do(httpRequest)
	if err != nil {
		return testMCPResponse{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return testMCPResponse{}, fmt.Errorf("HTTP status %s", response.Status)
	}
	var decoded testMCPResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return testMCPResponse{}, err
	}
	if decoded.Error != nil {
		return testMCPResponse{}, fmt.Errorf("MCP error %d: %s", decoded.Error.Code, decoded.Error.Message)
	}
	return decoded, nil
}

func testMCPInitializeRequest(id int) testMCPRequest {
	return testMCPRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  "initialize",
		Params: map[string]interface{}{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]interface{}{},
			"clientInfo": map[string]string{
				"name":    "goobers-test",
				"version": "1",
			},
		},
	}
}

func testMCPToolRequest(id int) testMCPRequest {
	return testMCPRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  "tools/call",
		Params: map[string]interface{}{
			"name":      "reachability",
			"arguments": map[string]interface{}{},
		},
	}
}

func testMCPInitializeResponse(id int) map[string]interface{} {
	return map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"result": map[string]interface{}{
			"protocolVersion": "2025-06-18",
			"capabilities": map[string]interface{}{
				"tools": map[string]interface{}{},
			},
			"serverInfo": map[string]string{
				"name":    "goobers-test-server",
				"version": "1",
			},
		},
	}
}

func testMCPToolResponse(id int, text string) map[string]interface{} {
	return map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"result": map[string]interface{}{
			"content": []map[string]string{{
				"type": "text",
				"text": text,
			}},
		},
	}
}

func testMCPToolText(response testMCPResponse) (string, error) {
	if len(response.Result.Content) != 1 || response.Result.Content[0].Type != "text" {
		return "", fmt.Errorf("unexpected MCP tool result: %#v", response.Result)
	}
	return response.Result.Content[0].Text, nil
}

func newTestRemoteMCPServer(t *testing.T, result, authorization string, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method != http.MethodPost ||
			request.Header.Get("Content-Type") != "application/json" ||
			!strings.Contains(request.Header.Get("Accept"), "application/json") {
			http.Error(writer, "invalid MCP HTTP request", http.StatusBadRequest)
			return
		}
		if got := request.Header.Get("Authorization"); got != authorization {
			http.Error(writer, "invalid authorization", http.StatusUnauthorized)
			return
		}
		var rpcRequest testMCPRequest
		if err := json.NewDecoder(request.Body).Decode(&rpcRequest); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		if rpcRequest.JSONRPC != "2.0" {
			http.Error(writer, "invalid JSON-RPC version", http.StatusBadRequest)
			return
		}
		switch rpcRequest.Method {
		case "initialize":
			_ = json.NewEncoder(writer).Encode(testMCPInitializeResponse(rpcRequest.ID))
		case "tools/call":
			if !isReachabilityToolCall(rpcRequest) {
				http.Error(writer, "unexpected MCP tool call", http.StatusBadRequest)
				return
			}
			calls.Add(1)
			_ = json.NewEncoder(writer).Encode(testMCPToolResponse(rpcRequest.ID, result))
		default:
			http.Error(writer, "unexpected MCP method", http.StatusBadRequest)
		}
	}))
}

func isReachabilityToolCall(request testMCPRequest) bool {
	params, ok := request.Params.(map[string]interface{})
	return ok && params["name"] == "reachability"
}

func writeTestCopilotMCPConfig(t *testing.T, home string, servers map[string]testCopilotMCPServer) {
	t.Helper()
	writeTestCopilotMCPConfigFile(t, filepath.Join(home, "mcp-config.json"), servers)
}

func writeTestCopilotMCPConfigFile(t *testing.T, path string, servers map[string]testCopilotMCPServer) {
	t.Helper()
	data, err := json.Marshal(testCopilotMCPConfig{MCPServers: servers})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeTestCopilotConfig(t *testing.T, home string, plugins []testCopilotInstalledPlugin) {
	t.Helper()
	data, err := json.Marshal(testCopilotConfig{
		OAuthToken:       "stored-auth",
		InstalledPlugins: plugins,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readTestCopilotInstalledPlugins(home string) ([]testCopilotInstalledPlugin, error) {
	data, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var config testCopilotConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	return config.InstalledPlugins, nil
}

func readTestCopilotMCPConfig(path string) (map[string]testCopilotMCPServer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var config testCopilotMCPConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	return config.MCPServers, nil
}

func assertMCPResults(t *testing.T, payload []byte, want map[string]string) {
	t.Helper()
	var result struct {
		Outputs map[string]string `json:"outputs"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Outputs) != len(want) {
		t.Fatalf("MCP outputs = %v, want %v", result.Outputs, want)
	}
	for name, value := range want {
		if result.Outputs[name] != value {
			t.Fatalf("MCP output %q = %q, want %q", name, result.Outputs[name], value)
		}
	}
}

func assertStdioMCPCalls(t *testing.T, path string, want int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "called\n"); got != want {
		t.Fatalf("stdio MCP tool calls = %d, want %d", got, want)
	}
}
