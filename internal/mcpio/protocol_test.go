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
		Inputs:       map[string]string{"churn-data.artifact[0]": ".goobers/context/00-churn-data.artifact_0_"},
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
	in.WriteString(rpcLine(t, 1, "initialize", map[string]interface{}{}))
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

	var out bytes.Buffer
	var errBuf bytes.Buffer
	if err := srv.Serve(&in, &out, &errBuf); err != nil {
		t.Fatalf("Serve: %v (stderr: %s)", err, errBuf.String())
	}
	if errBuf.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", errBuf.String())
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 5 {
		t.Fatalf("expected 5 responses (init, list, 3 calls), got %d: %v", len(lines), lines)
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

	// tools/list — 3 tools present
	listResult, _ := json.Marshal(responses[1].Result)
	if !strings.Contains(string(listResult), "publish_output") ||
		!strings.Contains(string(listResult), "list_inputs") ||
		!strings.Contains(string(listResult), "read_input") {
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
