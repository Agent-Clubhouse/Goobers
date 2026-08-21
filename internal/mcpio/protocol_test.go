package mcpio

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	ws := t.TempDir()
	contextDir := filepath.Join(ws, ".goobers", "context")
	if err := os.MkdirAll(contextDir, 0o755); err != nil {
		t.Fatal(err)
	}
	upstream := filepath.Join(contextDir, "00-churn-data.artifact_0_")
	if err := os.WriteFile(upstream, []byte("upstream content"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Workspace:    ws,
		ArtifactFile: "findings.md",
		ReceiptFile:  filepath.Join(".goobers", "mcp-io", ReceiptFileName),
		Inputs:       map[string]string{"churn-data.artifact[0]": ".goobers/context/00-churn-data.artifact_0_"},
		RunID:        "run-123",
		WorkflowID:   "implementation",
		TaskID:       "implement",
		Gaggle:       "goobers",
	}
	return NewServer(NewToolset(cfg)), ws
}

func rpcLine(t *testing.T, id int, method string, params interface{}) string {
	t.Helper()
	req := map[string]interface{}{"jsonrpc": "2.0", "method": method}
	if id >= 0 {
		req["id"] = id
	}
	if params != nil {
		req["params"] = params
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return string(data) + "\n"
}

func TestServeFullSession(t *testing.T) {
	srv, ws := newTestServer(t)

	var in bytes.Buffer
	in.WriteString(rpcLine(t, 1, "initialize", map[string]interface{}{"protocolVersion": "2025-06-18"}))
	in.WriteString(rpcLine(t, -1, "notifications/initialized", nil))
	in.WriteString(rpcLine(t, 2, "tools/list", map[string]interface{}{}))
	in.WriteString(rpcLine(t, 3, "tools/call", map[string]interface{}{
		"name":      "publish_output",
		"arguments": map[string]interface{}{"content": "# Findings\n\nfound stuff"},
	}))
	in.WriteString(rpcLine(t, 4, "tools/call", map[string]interface{}{
		"name": "list_inputs",
	}))
	in.WriteString(rpcLine(t, 5, "tools/call", map[string]interface{}{
		"name":      "read_input",
		"arguments": map[string]interface{}{"name": "churn-data.artifact[0]"},
	}))
	in.WriteString(rpcLine(t, 6, "tools/call", map[string]interface{}{
		"name": "get_run_info",
	}))

	var out bytes.Buffer
	var errBuf bytes.Buffer
	if err := srv.Serve(&in, &out, &errBuf); err != nil {
		t.Fatalf("Serve: %v (stderr: %s)", err, errBuf.String())
	}
	// stderr carries exactly one line now — the once-per-session negotiated
	// protocol version (#3457). Anything else on stderr is still a failure.
	wantStderr := "mcpio: MCP protocol version negotiated: requested=2025-06-18 agreed=2025-06-18\n"
	if errBuf.String() != wantStderr {
		t.Fatalf("stderr = %q, want %q", errBuf.String(), wantStderr)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 6 {
		t.Fatalf("expected 6 responses (init, list, 4 calls), got %d: %v", len(lines), lines)
	}

	var responses []rpcResponse
	for _, line := range lines {
		var r rpcResponse
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("bad response line %q: %v", line, err)
		}
		responses = append(responses, r)
	}

	// initialize
	if responses[0].Error != nil {
		t.Fatalf("initialize errored: %+v", responses[0].Error)
	}

	// tools/list — expected tools present
	listResult, _ := json.Marshal(responses[1].Result)
	if !strings.Contains(string(listResult), "publish_output") ||
		!strings.Contains(string(listResult), "list_inputs") ||
		!strings.Contains(string(listResult), "read_input") ||
		!strings.Contains(string(listResult), "get_run_info") {
		t.Fatalf("tools/list missing an expected tool: %s", listResult)
	}

	// publish_output — actually wrote the file
	written, err := os.ReadFile(filepath.Join(ws, "findings.md"))
	if err != nil {
		t.Fatalf("findings.md not written: %v", err)
	}
	if string(written) != "# Findings\n\nfound stuff" {
		t.Fatalf("unexpected file content: %q", written)
	}
	publishResult, _ := json.Marshal(responses[2].Result)
	if !strings.Contains(string(publishResult), `status`) || !strings.Contains(string(publishResult), `ok`) {
		t.Fatalf("publish_output result missing status ok: %s", publishResult)
	}

	// list_inputs — names our one seeded input
	listInputsResult, _ := json.Marshal(responses[3].Result)
	if !strings.Contains(string(listInputsResult), "churn-data.artifact[0]") {
		t.Fatalf("list_inputs missing seeded input: %s", listInputsResult)
	}

	// read_input — returns its content
	readInputResult, _ := json.Marshal(responses[4].Result)
	if !strings.Contains(string(readInputResult), "upstream content") {
		t.Fatalf("read_input missing content: %s", readInputResult)
	}

	// get_run_info — returns the identity from the invocation config
	runInfoResult, _ := json.Marshal(responses[5].Result)
	for _, want := range []string{"run-123", "implementation", "implement", "goobers"} {
		if !strings.Contains(string(runInfoResult), want) {
			t.Fatalf("get_run_info missing %q: %s", want, runInfoResult)
		}
	}

	receipts, err := ReadInputInspectionReceipts(ws, filepath.Join(".goobers", "mcp-io", ReceiptFileName))
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 2 {
		t.Fatalf("input inspection receipts = %+v, want list_inputs and read_input", receipts)
	}
	if receipts[0].Tool != "list_inputs" || !receipts[0].Success {
		t.Fatalf("list_inputs receipt = %+v", receipts[0])
	}
	if receipts[1].Tool != "read_input" || receipts[1].Input != "churn-data.artifact[0]" ||
		!receipts[1].Success || !strings.HasPrefix(receipts[1].InputDigest, "sha256:") ||
		receipts[1].StartLine != 1 || receipts[1].EndLine != 1 {
		t.Fatalf("read_input receipt = %+v", receipts[1])
	}
}

func TestGrepInputRecordsMatches(t *testing.T) {
	srv, ws := newTestServer(t)
	var out bytes.Buffer
	if err := srv.Serve(strings.NewReader(rpcLine(t, 1, "tools/call", map[string]interface{}{
		"name": "grep_input",
		"arguments": map[string]interface{}{
			"name": "churn-data.artifact[0]", "pattern": "upstream", "contextLines": 1,
		},
	})), &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	receipts, err := ReadInputInspectionReceipts(ws, filepath.Join(".goobers", "mcp-io", ReceiptFileName))
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 1 || receipts[0].Tool != "grep_input" ||
		receipts[0].Input != "churn-data.artifact[0]" || !receipts[0].Success ||
		!strings.HasPrefix(receipts[0].InputDigest, "sha256:") ||
		len(receipts[0].MatchLines) != 1 || receipts[0].MatchLines[0] != 1 {
		t.Fatalf("grep_input receipts = %+v", receipts)
	}
}

// TestInitializeNegotiatesProtocolVersion is the regression test for #3457.
// The server used to answer any protocolVersion outside its own list with
// -32602, which the MCP lifecycle forbids — a server that doesn't support
// the requested revision MUST answer with one it does support and let the
// client decide. The old behaviour wasn't a pedantic violation: Copilot CLI
// 1.0.80 requests a revision newer than the list, the handshake failed, and
// every agentic stage on that build lost the whole goobers-io toolset. The
// error is now reserved for an initialize carrying no usable
// protocolVersion at all, where there is nothing to negotiate from.
func TestInitializeNegotiatesProtocolVersion(t *testing.T) {
	newest := supportedProtocolVersions[0]

	tests := []struct {
		name string
		// params is what rides in the initialize request; nil omits the
		// params member entirely.
		params interface{}
		// wantVersion is the protocolVersion the initialize result must
		// echo. Empty means the case must fail instead.
		wantVersion string
		// wantError is an exact error-message match; wantErrorPrefix a
		// prefix match, for messages that wrap a json decoder error.
		wantError       string
		wantErrorPrefix string
	}{
		{
			name:        "newest supported echoed verbatim",
			params:      map[string]interface{}{"protocolVersion": "2025-11-25"},
			wantVersion: "2025-11-25",
		},
		{
			name:        "previous supported echoed verbatim, not upgraded to newest",
			params:      map[string]interface{}{"protocolVersion": "2025-06-18"},
			wantVersion: "2025-06-18",
		},
		{
			name:        "oldest supported echoed verbatim, not upgraded to newest",
			params:      map[string]interface{}{"protocolVersion": "2024-11-05"},
			wantVersion: "2024-11-05",
		},
		{
			// The shape of the next CLI bump after the one that caused
			// #3457: a revision that does not exist yet must negotiate, not
			// fail, or this outage simply recurs.
			name:        "unknown newer version negotiates down to newest supported",
			params:      map[string]interface{}{"protocolVersion": "2026-11-25"},
			wantVersion: newest,
		},
		{
			name:        "unknown older version negotiates up to newest supported",
			params:      map[string]interface{}{"protocolVersion": "2024-01-01"},
			wantVersion: newest,
		},
		{
			// The spec's own example of a client that isn't speaking dated
			// revisions at all. Still a well-formed string, so still a
			// negotiation input rather than an error.
			name:        "unrecognized version scheme negotiates to newest supported",
			params:      map[string]interface{}{"protocolVersion": "1.0.0"},
			wantVersion: newest,
		},
		{
			name:      "absent params",
			params:    nil,
			wantError: "initialize requires protocolVersion",
		},
		{
			name:      "missing protocolVersion",
			params:    map[string]interface{}{},
			wantError: "initialize requires protocolVersion",
		},
		{
			name:      "empty protocolVersion",
			params:    map[string]interface{}{"protocolVersion": ""},
			wantError: "initialize requires protocolVersion",
		},
		{
			name:            "malformed protocolVersion type",
			params:          map[string]interface{}{"protocolVersion": 20251125},
			wantErrorPrefix: "invalid initialize params:",
		},
		{
			name:            "malformed params shape",
			params:          []interface{}{"2025-11-25"},
			wantErrorPrefix: "invalid initialize params:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := newTestServer(t)
			var out, errBuf bytes.Buffer
			if err := srv.Serve(
				strings.NewReader(rpcLine(t, 1, "initialize", tt.params)),
				&out,
				&errBuf,
			); err != nil {
				t.Fatal(err)
			}

			var response rpcResponse
			if err := json.Unmarshal(out.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if tt.wantError != "" || tt.wantErrorPrefix != "" {
				if response.Error == nil {
					t.Fatalf("expected protocol error, got result %#v", response.Result)
				}
				if response.Error.Code != -32602 {
					t.Fatalf("error code = %d, want -32602 (error: %+v)", response.Error.Code, response.Error)
				}
				if tt.wantError != "" && response.Error.Message != tt.wantError {
					t.Fatalf("error message = %q, want %q", response.Error.Message, tt.wantError)
				}
				if tt.wantErrorPrefix != "" && !strings.HasPrefix(response.Error.Message, tt.wantErrorPrefix) {
					t.Fatalf("error message = %q, want prefix %q", response.Error.Message, tt.wantErrorPrefix)
				}
				if errBuf.Len() != 0 {
					t.Fatalf("nothing was negotiated, so nothing should be logged; got stderr %q", errBuf.String())
				}
				return
			}
			if response.Error != nil {
				t.Fatalf("unexpected protocol error: %+v", response.Error)
			}
			result, ok := response.Result.(map[string]interface{})
			if !ok {
				t.Fatalf("result type = %T, want map", response.Result)
			}
			if got := result["protocolVersion"]; got != tt.wantVersion {
				t.Fatalf("protocolVersion = %v, want %q", got, tt.wantVersion)
			}

			// The negotiated version is reported on our own side of the
			// wire, not only in the client's private log — see #3457's
			// note on why the outage stayed invisible for seven occurrences.
			sent, _ := tt.params.(map[string]interface{})
			requested, _ := sent["protocolVersion"].(string)
			wantLog := "requested=" + requested + " agreed=" + tt.wantVersion
			if !strings.Contains(errBuf.String(), wantLog) {
				t.Fatalf("stderr = %q, want it to record %q", errBuf.String(), wantLog)
			}
		})
	}
}

// TestEverySupportedProtocolVersionIsEchoedVerbatim pins the half of the
// negotiation contract that the fallback could otherwise mask: a version the
// server genuinely supports must come back unchanged, never silently
// upgraded to the newest one. Driven off supportedProtocolVersions itself so
// that adding a revision cannot leave a gap here.
func TestEverySupportedProtocolVersionIsEchoedVerbatim(t *testing.T) {
	if len(supportedProtocolVersions) == 0 {
		t.Fatal("supportedProtocolVersions is empty; negotiation has nothing to offer")
	}
	for _, version := range supportedProtocolVersions {
		t.Run(version, func(t *testing.T) {
			srv, _ := newTestServer(t)
			var out bytes.Buffer
			if err := srv.Serve(
				strings.NewReader(rpcLine(t, 1, "initialize", map[string]interface{}{"protocolVersion": version})),
				&out,
				&bytes.Buffer{},
			); err != nil {
				t.Fatal(err)
			}
			var response rpcResponse
			if err := json.Unmarshal(out.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Error != nil {
				t.Fatalf("supported version %q errored: %+v", version, response.Error)
			}
			result, ok := response.Result.(map[string]interface{})
			if !ok {
				t.Fatalf("result type = %T, want map", response.Result)
			}
			if got := result["protocolVersion"]; got != version {
				t.Fatalf("protocolVersion = %v, want %q echoed back verbatim", got, version)
			}
		})
	}
}

// TestNegotiationIsLoggedOncePerSession keeps the new stderr line from
// becoming per-request noise in a long stage's harness output if a client
// re-initializes.
func TestNegotiationIsLoggedOncePerSession(t *testing.T) {
	srv, _ := newTestServer(t)
	var in bytes.Buffer
	in.WriteString(rpcLine(t, 1, "initialize", map[string]interface{}{"protocolVersion": "2025-11-25"}))
	in.WriteString(rpcLine(t, 2, "initialize", map[string]interface{}{"protocolVersion": "2025-11-25"}))

	var out, errBuf bytes.Buffer
	if err := srv.Serve(&in, &out, &errBuf); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(errBuf.String(), "MCP protocol version negotiated"); got != 1 {
		t.Fatalf("negotiation logged %d times, want exactly 1: %q", got, errBuf.String())
	}
}

func TestPublishOutputWithoutArtifactFileErrors(t *testing.T) {
	ws := t.TempDir()
	srv := NewServer(NewToolset(Config{Workspace: ws}))

	var in bytes.Buffer
	in.WriteString(rpcLine(t, 1, "tools/call", map[string]interface{}{
		"name":      "publish_output",
		"arguments": map[string]interface{}{"content": "x"},
	}))
	var out, errBuf bytes.Buffer
	if err := srv.Serve(&in, &out, &errBuf); err != nil {
		t.Fatal(err)
	}
	var resp rpcResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	result, _ := json.Marshal(resp.Result)
	if !strings.Contains(string(result), `"isError":true`) {
		t.Fatalf("expected isError result, got %s", result)
	}
}

func TestResolveInWorkspaceRejectsEscape(t *testing.T) {
	ws := t.TempDir()
	tool := NewToolset(Config{Workspace: ws, ArtifactFile: "../../etc/passwd"})
	if _, err := tool.PublishOutput("x"); err == nil {
		t.Fatal("expected escape to be rejected")
	}
}

// TestPublishOutputRefusesToFollowAnExistingSymlinkLeaf is a direct
// regression test for a review finding on #2406: resolveInWorkspace only
// checked the workspace root and the target's parent directory for a
// symlink pointing outside — never the leaf itself. An attacker-planted
// symlink at the exact declared artifactFile path (e.g. via the model's own
// bash tool, out.md -> /outside/file) passed containment lexically, and
// os.WriteFile follows a symlink at open() time, truncating whatever it
// actually points to. This proves both that the write is now rejected and,
// just as importantly, that the outside file is never touched.
func TestPublishOutputRefusesToFollowAnExistingSymlinkLeaf(t *testing.T) {
	ws := t.TempDir()
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "sensitive")
	if err := os.WriteFile(outsideFile, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(ws, "out.md")
	if err := os.Symlink(outsideFile, leaf); err != nil {
		t.Fatal(err)
	}

	tool := NewToolset(Config{Workspace: ws, ArtifactFile: "out.md"})
	if _, err := tool.PublishOutput("attacker-controlled content"); err == nil {
		t.Fatal("expected publish_output to refuse to write through an existing symlink")
	}

	data, err := os.ReadFile(outsideFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "original" {
		t.Fatalf("outside file was modified through the symlink: %q", data)
	}
}

// TestReadInputRefusesToFollowAnExistingSymlinkLeaf covers the same gap on
// the read side — an input path that was replaced with a symlink between
// materializeContext writing it and a later read_input call (or a
// maliciously named input mapping) must not be followed either.
func TestReadInputRefusesToFollowAnExistingSymlinkLeaf(t *testing.T) {
	ws := t.TempDir()
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret")
	if err := os.WriteFile(outsideFile, []byte("do not leak"), 0o644); err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(ws, "in.txt")
	if err := os.Symlink(outsideFile, leaf); err != nil {
		t.Fatal(err)
	}

	tool := NewToolset(Config{Workspace: ws, Inputs: map[string]string{"x": "in.txt"}})
	if _, err := tool.ReadInput("x", 0, 0); err == nil {
		t.Fatal("expected read_input to refuse to read through an existing symlink")
	}
}

// TestPublishOutputRefusesToTraverseANestedSymlinkedAncestor is a regression
// test for a second review finding: for an artifactFile like
// "link/new/out.md" where "link" is a symlink pointing outside the
// workspace and "new" doesn't exist yet, the original fix's leaf-only check
// missed it — filepath.EvalSymlinks(dir) fails (dir doesn't exist), that
// error was silently ignored, and os.MkdirAll then walked straight through
// "link" (MkdirAll follows symlinks at existing intermediate components)
// and created "new" — and later wrote out.md — outside the workspace. This
// proves the write is now rejected before anything is created, and that
// nothing was ever created outside the workspace.
func TestPublishOutputRefusesToTraverseANestedSymlinkedAncestor(t *testing.T) {
	ws := t.TempDir()
	outsideDir := t.TempDir()
	link := filepath.Join(ws, "link")
	if err := os.Symlink(outsideDir, link); err != nil {
		t.Fatal(err)
	}

	tool := NewToolset(Config{Workspace: ws, ArtifactFile: "link/new/out.md"})
	if _, err := tool.PublishOutput("attacker-controlled content"); err == nil {
		t.Fatal("expected publish_output to refuse to traverse a symlinked ancestor")
	}

	if _, err := os.Lstat(filepath.Join(outsideDir, "new")); !os.IsNotExist(err) {
		t.Fatalf("directory was created outside the workspace through the symlink: err=%v", err)
	}
}

// TestReadInputRefusesToTraverseANestedSymlinkedAncestor covers the same
// gap on the read side: an input path whose intermediate directory is a
// symlink pointing outside the workspace must be rejected even though the
// file at the far end genuinely exists.
func TestReadInputRefusesToTraverseANestedSymlinkedAncestor(t *testing.T) {
	ws := t.TempDir()
	outsideDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outsideDir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	outsideFile := filepath.Join(outsideDir, "nested", "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("do not leak"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(ws, "link")
	if err := os.Symlink(outsideDir, link); err != nil {
		t.Fatal(err)
	}

	tool := NewToolset(Config{Workspace: ws, Inputs: map[string]string{"x": "link/nested/secret.txt"}})
	if _, err := tool.ReadInput("x", 0, 0); err == nil {
		t.Fatal("expected read_input to refuse to traverse a symlinked ancestor")
	}
}
