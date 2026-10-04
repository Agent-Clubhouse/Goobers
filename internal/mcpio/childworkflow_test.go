package mcpio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract/childworkflowwire"
)

type childRoundTripper func(*http.Request) (*http.Response, error)

func (f childRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const childMCPGrant = "goobers-child.test-payload.test-signature"

func newChildServer(t *testing.T) (*Server, string) {
	t.Helper()
	server, ws := newTestServer(t)
	server.tools.cfg.ChildWorkflows = &ChildWorkflowAccess{Endpoint: "https://daemon.invalid", BearerToken: childMCPGrant}
	if err := os.WriteFile(filepath.Join(ws, "proposal.yaml"), []byte("kind: Workflow\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return server, ws
}

func childRPC(t *testing.T, server *Server, name string, args json.RawMessage) (map[string]any, string) {
	t.Helper()
	var out, stderr bytes.Buffer
	input := rpcLine(t, 1, "tools/call", map[string]any{"name": name, "arguments": args})
	if err := server.Serve(strings.NewReader(input), &out, &stderr); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Result map[string]any `json:"result"`
		Error  any            `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error != nil {
		t.Fatalf("protocol error: %s", out.String())
	}
	if strings.Contains(out.String()+stderr.String(), childMCPGrant) {
		t.Fatal("grant leaked in MCP result or stderr")
	}
	return response.Result, out.String()
}

func childHTTPResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestChildToolsUnavailableWithoutTrustedAccess(t *testing.T) {
	server, _ := newTestServer(t)
	for _, name := range []string{"validate_child_workflow", "start_child_workflow", "get_child_workflow"} {
		result, _ := childRPC(t, server, name, json.RawMessage(`{"sourceFile":"proposal.yaml","invocationKey":"inspect"}`))
		if result["isError"] != true {
			t.Fatalf("ungranted %s succeeded", name)
		}
	}
	for _, def := range server.toolDefs() {
		if isChildWorkflowTool(def.Name) {
			t.Fatal("ungranted child tool advertised")
		}
	}
}

func TestChildToolsRealProtocolAndTrustedHTTP(t *testing.T) {
	server, _ := newChildServer(t)
	var seen []string
	server.tools.childTransport = childRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.URL.Scheme != "https" || request.URL.Host != "daemon.invalid" || request.Header.Get("Authorization") != "Bearer "+childMCPGrant || request.Method != http.MethodPost {
			t.Fatal("lost trusted target or grant")
		}
		deadline, ok := request.Context().Deadline()
		if !ok || time.Until(deadline) > childWorkflowTimeout {
			t.Fatal("missing transport bound")
		}
		data, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		path := request.URL.Path
		seen = append(seen, path)
		if strings.HasSuffix(path, "/status") {
			var body childworkflowwire.ChildWorkflowStatusRequest
			if err := json.Unmarshal(data, &body); err != nil || body.InvocationKey != "inspect-1" {
				t.Fatalf("body=%s", data)
			}
			return childHTTPResponse(200, `{"state":"queued","invocationKey":"inspect-1"}`), nil
		}
		var body childworkflowwire.ChildWorkflowSourceRequest
		if err := json.Unmarshal(data, &body); err != nil || body.Source != "kind: Workflow\n" {
			t.Fatalf("source body=%s", data)
		}
		if strings.HasSuffix(path, "/start") {
			if request.Header.Get("Idempotency-Key") != "inspect-1" {
				t.Fatal("start lost invocation key")
			}
			if bytes.Contains(data, []byte("invocationKey")) {
				t.Fatal("key leaked into start source body")
			}
			return childHTTPResponse(202, `{"state":"queued","invocationKey":"inspect-1"}`), nil
		}
		if request.Header.Get("Idempotency-Key") != "" {
			t.Fatal("validate gained key")
		}
		return childHTTPResponse(200, `{"valid":true,"advisory":true,"diagnostics":[]}`), nil
	})
	for name, args := range map[string]string{"validate_child_workflow": `{"sourceFile":"proposal.yaml"}`, "start_child_workflow": `{"sourceFile":"proposal.yaml","invocationKey":"inspect-1"}`, "get_child_workflow": `{"invocationKey":"inspect-1"}`} {
		result, out := childRPC(t, server, name, json.RawMessage(args))
		if result["isError"] == true {
			t.Fatalf("%s failed: %s", name, out)
		}
	}
	if len(seen) != 3 {
		t.Fatalf("calls=%v", seen)
	}
	for _, path := range seen {
		if !strings.HasPrefix(path, "/api/v1/runs/run-123/child-workflows/") {
			t.Fatal("scope changed")
		}
	}
	names := []string{}
	for _, def := range server.toolDefs() {
		names = append(names, def.Name)
	}
	if len(names) != 9 {
		t.Fatalf("tools=%v", names)
	}
	if strings.Contains(strings.Join(names, ","), "await") {
		t.Fatal("unsupported operations advertised")
	}
}

func TestChildToolsRejectUntrustedArgumentsBeforeHTTP(t *testing.T) {
	server, _ := newChildServer(t)
	server.tools.childTransport = childRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid arguments reached HTTP")
		return nil, nil
	})
	for _, args := range []string{`{}`, `null`, `[]`, `{"sourceFile":null}`, `{"sourceFile":1}`, `{"sourceFile":""}`, `{"SourceFile":"proposal.yaml"}`, `{"sourceFile":"proposal.yaml","sourceFile":"other"}`, `{"sourceFile":"proposal.yaml","run":"sibling"}`, `{"sourceFile":"proposal.yaml","policy":{}}`, `{"sourceFile":"proposal.yaml","endpoint":"https://evil.invalid"}`, `{"sourceFile":"proposal.yaml","bearerToken":"forged"}`, `{"sourceFile":"proposal.yaml","invocationKey":"unexpected"}`} {
		result, _ := childRPC(t, server, "validate_child_workflow", json.RawMessage(args))
		if result["isError"] != true {
			t.Fatalf("accepted %s", args)
		}
	}
	for _, key := range []string{"", " key ", "x\u0085y", strings.Repeat("x", 257)} {
		args, _ := json.Marshal(map[string]string{"sourceFile": "proposal.yaml", "invocationKey": key})
		result, _ := childRPC(t, server, "start_child_workflow", args)
		if result["isError"] != true {
			t.Fatal("invalid key accepted")
		}
	}
}

func TestChildSourceWorkspaceConfinementAndBounds(t *testing.T) {
	server, ws := newChildServer(t)
	outside := filepath.Join(t.TempDir(), "outside.yaml")
	if err := os.WriteFile(outside, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "escape.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".goobers", "mcp-io"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".goobers", "mcp-io", ConfigFileName), []byte(childMCPGrant), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ws, ".goobers"), filepath.Join(ws, "runtime-alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "large.yaml"), bytes.Repeat([]byte("x"), childworkflowwire.MaxChildWorkflowSourceBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "bad.yaml"), []byte{0xff}, 0o600); err != nil {
		t.Fatal(err)
	}
	server.tools.childTransport = childRoundTripper(func(*http.Request) (*http.Response, error) { t.Fatal("unsafe file reached HTTP"); return nil, nil })
	for _, path := range []string{"../outside.yaml", outside, `C:\outside.yaml`, `\\host\share`, "escape.yaml", ".", "missing.yaml", "large.yaml", "bad.yaml", ".goobers/mcp-io/" + ConfigFileName, "runtime-alias/mcp-io/" + ConfigFileName, ".git/config"} {
		args, _ := json.Marshal(map[string]string{"sourceFile": path})
		result, _ := childRPC(t, server, "validate_child_workflow", args)
		if result["isError"] != true {
			t.Fatalf("accepted path=%s", path)
		}
	}
	if err := os.WriteFile(filepath.Join(ws, "limit.yaml"), bytes.Repeat([]byte("x"), childworkflowwire.MaxChildWorkflowSourceBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	if source, err := server.tools.childWorkflowSource("limit.yaml"); err != nil || len(source) != childworkflowwire.MaxChildWorkflowSourceBytes {
		t.Fatalf("limit read err=%v len=%d", err, len(source))
	}
}

func TestChildTransportNoRedirectAndBoundedSafeErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		transport error
		want      string
	}{
		{name: "redirect", status: 307, body: "secret", want: "HTTP 307"},
		{name: "scope denied", status: 403, body: `{"error":{"code":"child_authority_unavailable","message":"` + childMCPGrant + `"}}`, want: "child_authority_unavailable"},
		{name: "conflict", status: 409, body: `{"error":{"code":"child_conflict"}}`, want: "child_conflict"},
		{name: "bad error code", status: 503, body: `{"error":{"code":"` + childMCPGrant + `"}}`, want: "request_failed"},
		{name: "bad response", status: 200, body: "{", want: "invalid child workflow response"},
		{name: "too large", status: 200, body: strings.Repeat("x", maxChildWorkflowResponseBytes+1), want: "exceeded"},
		{name: "transport", transport: errors.New(childMCPGrant), want: "transport failed"},
		{name: "timeout", transport: context.DeadlineExceeded, want: "transport failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := newChildServer(t)
			calls := 0
			server.tools.childTransport = childRoundTripper(func(*http.Request) (*http.Response, error) {
				calls++
				if calls > 1 {
					t.Fatal("followed redirect")
				}
				if tc.transport != nil {
					return nil, tc.transport
				}
				response := childHTTPResponse(tc.status, tc.body)
				response.Header.Set("Location", "https://evil.invalid/leak")
				return response, nil
			})
			result, out := childRPC(t, server, "get_child_workflow", json.RawMessage(`{"invocationKey":"inspect-1"}`))
			if result["isError"] != true || !strings.Contains(out, tc.want) {
				t.Fatalf("response=%s", out)
			}
		})
	}
	server, _ := newChildServer(t)
	server.tools.childTransport = childRoundTripper(func(*http.Request) (*http.Response, error) {
		return childHTTPResponse(200, `{"valid":false,"advisory":true,"diagnostics":[{"code":"schema","message":"`+childMCPGrant+`"}]}`), nil
	})
	result, out := childRPC(t, server, "validate_child_workflow", json.RawMessage(`{"sourceFile":"proposal.yaml"}`))
	if result["isError"] == true || !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("redaction response=%s", out)
	}
}

func TestChildConfigPrivateAndImmutable(t *testing.T) {
	root := t.TempDir()
	rel := ".goobers/mcp-io/" + ConfigFileName
	if err := os.MkdirAll(filepath.Join(root, ".goobers", "mcp-io"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, rel), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	access := &ChildWorkflowAccess{Endpoint: "http://127.0.0.1:1234", BearerToken: childMCPGrant}
	cfg := Config{Workspace: root, RunID: "run-123", ChildWorkflows: access}
	path, err := WriteConfig(root, rel, cfg)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode=%v err=%v", info, err)
	}
	loaded, err := LoadConfig(path)
	if err != nil || loaded.ChildWorkflows.BearerToken != childMCPGrant {
		t.Fatal("grant not delivered privately")
	}
	tools := NewToolset(cfg)
	access.Endpoint = "https://evil.invalid"
	if tools.cfg.ChildWorkflows.Endpoint == access.Endpoint {
		t.Fatal("mutable access pointer retained")
	}
	for _, rendered := range []string{fmt.Sprintf("%v", *access), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg)} {
		if strings.Contains(rendered, childMCPGrant) {
			t.Fatal("formatted diagnostic leaked token")
		}
	}
	for _, endpoint := range []string{"ftp://host", "https://user:password@host", "https://host/api", "https://host/?secret=x", "https://host/#secret", ":bad"} {
		bad := ChildWorkflowAccess{Endpoint: endpoint, BearerToken: childMCPGrant}
		if bad.Validate("run-123") == nil || len(ChildWorkflowToolNames(&bad, "run-123")) != 0 {
			t.Fatal("invalid endpoint accepted")
		}
	}
	if access.Validate("../sibling") == nil {
		t.Fatal("invalid run accepted")
	}
}

func TestChildToolsListUsesConditionalSchemas(t *testing.T) {
	server, _ := newChildServer(t)
	var out, stderr bytes.Buffer
	if err := server.Serve(strings.NewReader(rpcLine(t, 1, "tools/list", map[string]any{})), &out, &stderr); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Result struct {
			Tools []toolDef `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, tool := range response.Result.Tools {
		if !isChildWorkflowTool(tool.Name) {
			continue
		}
		found++
		schema := tool.InputSchema.(map[string]any)
		if schema["additionalProperties"] != false {
			t.Fatal("tool schema accepts authority overrides")
		}
		props := schema["properties"].(map[string]any)
		if props["endpoint"] != nil || props["runId"] != nil || props["bearerToken"] != nil {
			t.Fatal("tool exposes transport authority")
		}
	}
	if found != 4 {
		t.Fatalf("child tools=%d", found)
	}
	if strings.Contains(out.String()+stderr.String(), childMCPGrant) {
		t.Fatal("tools/list exposes grant")
	}
}
