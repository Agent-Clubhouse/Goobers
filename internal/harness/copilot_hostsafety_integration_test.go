//go:build integration && !windows

package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

// Execute through the real CLI's native authorization RPC, with no model
// turn. A successful control stub, a structured denial, and absent kill
// markers are all required; a model declining the instruction cannot pass.
func TestIntegrationCopilotDeniesNameBasedKill(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_COPILOT_HOSTSAFETY")
	testdep.Require(t, "copilot")
	workspace := t.TempDir()
	stubs := filepath.Join(workspace, "stubs")
	if err := os.Mkdir(stubs, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pkill", "killall", "goobers7394control"} {
		script := "#!/bin/sh\nprintf ran > '" + filepath.Join(workspace, name+".marker") + "'\n"
		if err := os.WriteFile(filepath.Join(stubs, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", stubs+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	info, err := (&CopilotAdapter{Command: []string{"copilot"}}).Preflight(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id, err := newHarnessSessionID()
	if err != nil {
		t.Fatal(err)
	}
	argv := withCopilotNoRemoteExport([]string{"copilot", "-p=", "--session-id", id, "--allow-all-tools", "--log-dir", filepath.Join(workspace, "logs")}, info.Version)
	argv = withCopilotHostSafety(argv)
	process, _, err := startCopilotControlProcess(ctx, ExecProcessRunner{}, ProcessRequest{
		Command: argv, Dir: workspace, Env: baseEnv(nil, nil), Timeout: time.Minute,
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer process.close()
	req := RunRequest{Workspace: workspace, Tools: []string{"shell"}}
	session, err := process.client.CreateSession(ctx, &copilot.SessionConfig{
		SessionID: id, WorkingDirectory: workspace, AvailableTools: []string{"bash"},
		DisabledMCPServers:  []string{"github-mcp-server"},
		OnPermissionRequest: copilotSessionPermissions(req, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	process.session = session
	if _, err := session.RPC.Tools.InitializeAndValidate(ctx); err != nil {
		t.Fatal(err)
	}
	execute := func(command string) *rpc.ToolResultExpanded {
		t.Helper()
		result, err := session.RPC.Tools.Execute(ctx, &rpc.ToolsExecuteRequest{
			Name: "bash", Arguments: map[string]any{"command": command, "description": "host process safety fixture"},
		})
		if err != nil {
			t.Fatal(err)
		}
		expanded := expandedMCPToolResult(result)
		if expanded == nil {
			t.Fatalf("missing structured native authorization result: %T", result)
		}
		return expanded
	}
	if result := execute("goobers7394control"); result.ResultType != rpc.ToolResultTypeSuccess {
		t.Fatalf("control refused: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(workspace, "goobers7394control.marker")); err != nil {
		t.Fatalf("control PATH did not reach harmless stubs: %v", err)
	}
	for _, command := range []string{"pkill -x fixture", "killall fixture", "bash -c 'pkill -x fixture'", "eval killall fixture"} {
		result := execute(command)
		// Newer CLI versions block bare name-kills inside the shell tool
		// itself and classify that refusal as failure, with a stable native
		// telemetry category, instead of a permission-decision result.
		telemetry, err := json.Marshal(result.ToolTelemetry)
		nativeKillDenied := err == nil && result.ResultType == rpc.ToolResultTypeFailure &&
			strings.Contains(string(telemetry), `"shell_error_category":"kill_command_blocked"`)
		if result.ResultType != rpc.ToolResultTypeDenied && result.ResultType != rpc.ToolResultTypeRejected && !nativeKillDenied {
			t.Fatalf("command %q not denied: %+v", command, result)
		}
	}
	for _, name := range []string{"pkill", "killall"} {
		if _, err := os.Stat(filepath.Join(workspace, name+".marker")); !os.IsNotExist(err) {
			t.Fatalf("forbidden stub %s executed (or marker unreadable): %v", name, err)
		}
	}
}
