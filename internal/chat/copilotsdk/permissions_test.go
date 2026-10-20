package copilotsdk

import (
	"strings"
	"testing"

	"github.com/github/copilot-sdk/go/rpc"
)

func TestPermissionPreviewUsesDisplayFieldsOnly(t *testing.T) {
	id := "tool-7"
	tests := []struct {
		req  rpc.PermissionRequest
		want string
	}{
		{&rpc.PermissionRequestShell{FullCommandText: "go test ./...", ToolCallID: &id}, "go test ./..."},
		{&rpc.PermissionRequestRead{Path: "README.md"}, "README.md"},
		{&rpc.PermissionRequestWrite{FileName: "a.go", Diff: "-old\n+new"}, "a.go\n-old\n+new"},
		{&rpc.PermissionRequestMCP{Args: map[string]any{"token": "must-not-copy"}}, ""},
		{&rpc.PermissionRequestShell{FullCommandText: strings.Repeat("x", 33<<10)}, ""},
	}
	for _, test := range tests {
		content, tool := permissionPreview(test.req)
		if test.want == "" {
			if len(content) != 0 {
				t.Fatal("opaque/oversized input became review content")
			}
		} else if len(content) != 1 || content[0].Text != test.want {
			t.Fatalf("preview = %+v", content)
		}
		if test.req.Kind() == rpc.PermissionRequestKindShell && test.want != "" && tool != id {
			t.Fatal("tool correlation missing")
		}
	}
}
