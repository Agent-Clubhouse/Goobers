//go:build integration

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

func TestIntegrationClaudeGoobersIOListInputsReceipt(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_CLAUDE_LIVE_SMOKE")
	testdep.Require(t, "claude")
	selfBin := os.Getenv("GOOBERS_CLAUDE_LIVE_SELF_BIN")
	if selfBin == "" {
		t.Fatal("GOOBERS_CLAUDE_LIVE_SELF_BIN must name the current goobers binary")
	}

	workspace := t.TempDir()
	inputName := "upstream.artifact[0]"
	inputPath := filepath.Join(workspace, "input.txt")
	if err := os.WriteFile(inputPath, []byte("probe input\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envelope := testEnvelope(workspace)
	envelope.Goal = "Call the mcp__goobers-io__list_inputs tool exactly once, then call mcp__goobers-io__publish_output with exactly this JSON: {\"status\":\"success\",\"summary\":\"listed inputs\"}. Do not use any other tool."
	envelope.Inputs = map[string]interface{}{InputArtifactFile: DefaultResultPath}
	adapter := &ClaudeAdapter{Command: []string{"claude"}, SelfBin: selfBin}
	out, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       envelope,
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		ContextPaths:   map[string]string{inputName: inputPath},
		Tools:          []string{"shell"},
		Timeout:        2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Run: %v (transcript: %s)", err, out.Transcript)
	}
	if !slices.ContainsFunc(out.InputInspectionReceipts, func(receipt mcpio.InputInspectionReceipt) bool {
		return receipt.Tool == "list_inputs" && receipt.Success
	}) {
		t.Fatalf("list_inputs receipt missing: %+v", out.InputInspectionReceipts)
	}
	if len(out.MCPServerFailures) != 0 {
		t.Fatalf("goobers-io MCP transport failure: %+v", out.MCPServerFailures)
	}
}
