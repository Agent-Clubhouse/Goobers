package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CopilotInvocationDiagnosticsFile is the workspace-relative path the Copilot
// adapter records the effective tool/permission invocation to (#2962).
const CopilotInvocationDiagnosticsFile = ".goobers/copilot-invocation.json"

// copilotPermissionFlagPrefixes are the argv flags that determine what the CLI
// will and will not let the model do. Recording exactly these — rather than
// the whole argv — keeps the prompt text and any credential-bearing arguments
// out of the diagnostics file.
var copilotPermissionFlagPrefixes = []string{
	"--allow-all-tools",
	"--allow-tool",
	"--deny-tool",
	"--available-tools",
	"--add-github-mcp-toolset",
	"--disable-builtin-mcps",
	"--no-allow-all-tools",
}

// copilotInvocationDiagnostics is the recorded shape. Stable field names so
// operators and future regression tests can assert on it.
type copilotInvocationDiagnostics struct {
	// CLIVersion is the version startup preflight reported, or "" when
	// preflight did not run or reported nothing.
	CLIVersion string `json:"cliVersion"`
	// DeclaredTools is the goober's default-deny tool allowlist as configured.
	DeclaredTools []string `json:"declaredTools"`
	// AvailableTools is the tool set actually advertised to the CLI, which is
	// the allowlist after the adapter's own auto-wiring (goobers-io, etc).
	AvailableTools []string `json:"availableTools"`
	// PermissionArgs are the effective permission-relevant CLI arguments, in
	// invocation order.
	PermissionArgs []string `json:"permissionArgs"`
	// ToolConstrained reports whether the run advertised an explicit tool set
	// rather than relying on the CLI's defaults.
	ToolConstrained bool `json:"toolConstrained"`
	// Sandboxed reports whether the session was wrapped in a platform sandbox.
	Sandboxed bool `json:"sandboxed"`
}

// writeCopilotInvocationDiagnostics records the effective permission posture
// for this session next to the debug prompt. Best-effort in content (an empty
// version or tool list is recorded as such) but fail-closed on IO: the same
// .goobers directory was just proven writable by the prompt write, so a
// failure here means the workspace is broken and the session should not start.
func writeCopilotInvocationDiagnostics(req RunRequest, argv []string) error {
	diagnostics := copilotInvocationDiagnostics{
		CLIVersion:      req.HarnessVersion,
		DeclaredTools:   append([]string(nil), req.Tools...),
		PermissionArgs:  copilotPermissionArgs(argv),
		ToolConstrained: len(req.Tools) > 0,
		Sandboxed:       req.Sandbox != nil,
	}
	if diagnostics.ToolConstrained {
		diagnostics.AvailableTools = copilotAvailableTools(req)
	}
	encoded, err := json.MarshalIndent(diagnostics, "", "  ")
	if err != nil {
		return fmt.Errorf("encode invocation diagnostics: %w", err)
	}
	path := filepath.Join(req.Workspace, filepath.FromSlash(CopilotInvocationDiagnosticsFile))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("prepare invocation diagnostics dir: %w", err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
		return fmt.Errorf("write invocation diagnostics: %w", err)
	}
	return nil
}

// copilotPermissionArgs filters argv down to the permission-relevant flags,
// preserving invocation order. Both spellings are handled: "--flag=value"
// (kept whole, the value is a tool list, never a secret) and a bare "--flag".
func copilotPermissionArgs(argv []string) []string {
	var out []string
	for _, arg := range argv {
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name := arg
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name = name[:eq]
		}
		for _, prefix := range copilotPermissionFlagPrefixes {
			if name == prefix {
				out = append(out, arg)
				break
			}
		}
	}
	return out
}
