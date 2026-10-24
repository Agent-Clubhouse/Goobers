//go:build integration && !windows

package harness

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/mcpio"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// Exercise the installed CLI's actual SDK session and the built Goobers stdio
// server without sending a model prompt or mutating any child workflow.
func TestIntegrationCopilotParentMCPInventory(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_COPILOT_MCP_READINESS")
	testdep.RequireEnv(t, "GOOBERS_TEST_BINARY")
	testdep.Require(t, "copilot")
	binary, err := filepath.Abs(os.Getenv("GOOBERS_TEST_BINARY"))
	if err != nil {
		t.Fatal(err)
	}
	for _, parent := range []bool{true, false} {
		name := "ordinary"
		if parent {
			name = "parent"
		}
		t.Run(name, func(t *testing.T) {
			req := childHarnessRequest(t)
			if !parent {
				req.ChildWorkflows = nil
			}
			req = withAutoGoobersIO(req, binary)
			config, err := goobersIOAdditionalMCPConfigArg(req, binary)
			if err != nil {
				t.Fatal(err)
			}
			id, err := newHarnessSessionID()
			if err != nil {
				t.Fatal(err)
			}
			runner := &copilotControlledRunner{base: ExecProcessRunner{}, request: req, promptIndex: 1, mcpConfig: config}
			defer runner.close()
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			err = runner.initialize(ctx, ProcessRequest{Command: []string{"copilot", "-p=must not run", "--session-id", id, "--allow-all-tools", "--log-dir", filepath.Join(req.Workspace, "logs")}, Dir: req.Workspace, Env: overrideEnv(baseEnv(nil, nil), "COPILOT_HOME", filepath.Join(req.Workspace, "copilot-home"))})
			if err != nil {
				t.Fatal(err)
			}
			report, err := probeRequiredMCPSession(ctx, runner.session, 0)
			if err != nil || report.Connection != "ready" || report.Inventory != "ready" {
				t.Fatalf("MCP readiness: %+v %v", report, err)
			}
			inventory, err := runner.session.ListMCPTools(ctx, goobersIOServerName)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, tool := range inventory.Tools {
				names = append(names, tool.Name)
			}
			access := &mcpio.ChildWorkflowAccess{Endpoint: "https://daemon.invalid", BearerToken: "goobers-child.fixture"}
			for _, name := range mcpio.ChildWorkflowToolNames(access, req.Envelope.RunID) {
				if slices.Contains(names, name) != parent {
					t.Fatalf("parent=%v inventory lacks scoped tool %s: %v", parent, name, names)
				}
			}
		})
	}
}
