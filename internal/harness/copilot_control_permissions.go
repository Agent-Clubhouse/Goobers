package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"

	"github.com/goobers/goobers/internal/pathutil"
)

// The CLI's --allow-all-tools is NOT --allow-all-paths/--allow-all-urls.
// Preserve that distinction when answering native SDK permission requests.
// Managed approval and sandbox bypass always require a human, unavailable in
// this unattended adapter. The enclosing OS sandbox remains authoritative.
func copilotSessionPermissions(req RunRequest) copilot.PermissionHandlerFunc {
	available := copilotAvailableTools(req)
	return func(request copilot.PermissionRequest, invocation copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
		denied := &rpc.PermissionDecisionUserNotAvailable{}
		if request == nil || invocation.ManagedSettingsEnabled || request.RequiresManagedApproval() {
			return denied, nil
		}
		var detail copilotPermissionDetail
		data, err := json.Marshal(request)
		if err != nil || json.Unmarshal(data, &detail) != nil || detail.RequestSandboxBypass {
			return denied, nil
		}
		if !copilotPermissionAllowed(request.Kind(), detail, req.Workspace, available) {
			return denied, nil
		}
		return &rpc.PermissionDecisionApproveOnce{}, nil
	}
}

type copilotPermissionDetail struct {
	Path                 string            `json:"path"`
	FileName             string            `json:"fileName"`
	ServerName           string            `json:"serverName"`
	ToolName             string            `json:"toolName"`
	PossiblePaths        []string          `json:"possiblePaths"`
	PossibleURLs         []json.RawMessage `json:"possibleUrls"`
	RequestSandboxBypass bool              `json:"requestSandboxBypass"`
}

func copilotPermissionAllowed(kind rpc.PermissionRequestKind, detail copilotPermissionDetail, workspace string, available []string) bool {
	switch kind {
	case rpc.PermissionRequestKindMCP:
		name := detail.ToolName
		if !strings.HasPrefix(name, detail.ServerName+"-") {
			name = detail.ServerName + "-" + name
		}
		return slices.Contains(available, name)
	case rpc.PermissionRequestKindCustomTool:
		return slices.Contains(available, detail.ToolName)
	case rpc.PermissionRequestKindRead:
		return hasAnyCopilotTool(available, "view", "grep", "rg", "glob") && copilotPermissionPath(workspace, detail.Path)
	case rpc.PermissionRequestKindWrite:
		return hasAnyCopilotTool(available, "create", "edit", "str_replace_editor", "apply_patch") && copilotPermissionPath(workspace, detail.FileName)
	case rpc.PermissionRequestKindShell:
		if !hasAnyCopilotTool(available, "bash", "powershell") || len(detail.PossibleURLs) > 0 {
			return false
		}
		for _, path := range detail.PossiblePaths {
			if !copilotPermissionPath(workspace, path) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func hasAnyCopilotTool(available []string, names ...string) bool {
	for _, name := range names {
		if slices.Contains(available, name) {
			return true
		}
	}
	return false
}

// Resolve the nearest existing ancestor without creating files. Both lexical
// traversal and an existing symlink leading outside the workspace are denied.
func copilotPermissionPath(workspace, path string) bool {
	if path == "" {
		return false
	}
	root, err := filepath.Abs(workspace)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	if pathutil.ValidateSymlinkEscape(root, path, path) != nil && pathutil.ValidateSymlinkEscape(resolvedRoot, path, path) != nil {
		return false
	}
	candidate := path
	for {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err == nil {
			return pathutil.ValidateSymlinkEscape(resolvedRoot, resolved, path) == nil
		}
		if !os.IsNotExist(err) || candidate == root || filepath.Dir(candidate) == candidate {
			return false
		}
		candidate = filepath.Dir(candidate)
	}
}
