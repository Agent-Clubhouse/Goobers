package harness

import (
	"os"
	"path/filepath"
	"testing"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

func TestControlledCopilotPermissionsDoNotBroadenCLIGrants(t *testing.T) {
	workspace := t.TempDir()
	req := RunRequest{Workspace: workspace, Tools: append([]string{"shell"}, goobersIOAvailableToolNames()...)}
	runner := &copilotControlledRunner{request: req}
	handler := runner.sessionConfig("session", ProcessRequest{Dir: workspace}, nil).OnPermissionRequest
	yes := true
	for _, tc := range []struct {
		name    string
		request copilot.PermissionRequest
		allowed bool
	}{
		{"required readonly MCP", rpc.PermissionRequestMCP{ServerName: goobersIOServerName, ToolName: "get_run_info"}, true},
		{"undeclared MCP", rpc.PermissionRequestMCP{ServerName: "other", ToolName: "secret"}, false},
		{"workspace read", rpc.PermissionRequestRead{Path: workspace}, true},
		{"workspace new write", rpc.PermissionRequestWrite{FileName: filepath.Join(workspace, "new", "file")}, true},
		{"outside read", rpc.PermissionRequestRead{Path: t.TempDir()}, false},
		{"traversal write", rpc.PermissionRequestWrite{FileName: "../outside"}, false},
		{"workspace shell", rpc.PermissionRequestShell{PossiblePaths: []string{"."}}, true},
		{"outside shell", rpc.PermissionRequestShell{PossiblePaths: []string{t.TempDir()}}, false},
		{"shell url", rpc.PermissionRequestShell{PossibleURLs: []rpc.PermissionRequestShellPossibleURL{{URL: "https://example.invalid"}}}, false},
		{"url", rpc.PermissionRequestURL{URL: "https://example.invalid"}, false},
		{"sandbox bypass", rpc.PermissionRequestRead{Path: workspace, RequestSandboxBypass: &yes}, false},
		{"managed approval", rpc.PermissionRequestMCP{ServerName: goobersIOServerName, ToolName: "get_run_info", ManagedApprovalRequired: &yes}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision, err := handler(tc.request, copilot.PermissionInvocation{})
			_, allowed := decision.(*rpc.PermissionDecisionApproveOnce)
			if err != nil || allowed != tc.allowed {
				t.Fatalf("decision=%T err=%v", decision, err)
			}
		})
	}
	decision, err := handler(rpc.PermissionRequestRead{Path: workspace}, copilot.PermissionInvocation{ManagedSettingsEnabled: true})
	if _, approved := decision.(*rpc.PermissionDecisionApproveOnce); err != nil || approved {
		t.Fatalf("managed settings=%T %v", decision, err)
	}
}

func TestControlledCopilotPermissionRejectsSymlinkEscape(t *testing.T) {
	workspace, outside := t.TempDir(), t.TempDir()
	link := filepath.Join(workspace, "external")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if copilotPermissionPath(workspace, filepath.Join(link, "new", "file")) {
		t.Fatal("symlink escape authorized")
	}
	canonical, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !copilotPermissionPath(workspace, canonical) {
		t.Fatal("canonical workspace denied")
	}
}
