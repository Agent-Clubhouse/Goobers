//go:build integration

package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

// rawStdoutProcessRunner keeps each invocation's raw stdout, before the
// adapter converts it to the GenAI event schema and drops the CLI's own
// result fields (permission_denials among them).
type rawStdoutProcessRunner struct {
	inner ProcessRunner
	mu    sync.Mutex
	raw   [][]byte
}

func (r *rawStdoutProcessRunner) Run(ctx context.Context, req ProcessRequest) (ProcessResult, error) {
	result, err := r.inner.Run(ctx, req)
	r.mu.Lock()
	r.raw = append(r.raw, append([]byte(nil), result.Transcript...))
	r.mu.Unlock()
	return result, err
}

// claudeDeniedBashCommands returns the Bash commands a stream-json run's
// result events report as permission-denied.
func claudeDeniedBashCommands(t *testing.T, raw [][]byte) []string {
	t.Helper()
	var denied []string
	for _, stream := range raw {
		scanner := bufio.NewScanner(bytes.NewReader(stream))
		scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for scanner.Scan() {
			var event struct {
				Type              string `json:"type"`
				PermissionDenials []struct {
					ToolName  string `json:"tool_name"`
					ToolInput struct {
						Command string `json:"command"`
					} `json:"tool_input"`
				} `json:"permission_denials"`
			}
			if json.Unmarshal(scanner.Bytes(), &event) != nil || event.Type != "result" {
				continue
			}
			for _, denial := range event.PermissionDenials {
				if denial.ToolName == "Bash" {
					denied = append(denied, denial.ToolInput.Command)
				}
			}
		}
	}
	return denied
}

// TestIntegrationClaudeDeniesNameBasedKill drives a signed-in Claude Code
// through the adapter and checks that the CLI itself refuses a pkill on both
// invocation shapes (#6302). Nothing is ever signalled: pkill, killall and a
// control command are stubs first on PATH that only write marker files, and
// the requested command is `pkill -x <name no process has>`.
//
// Three facts must hold together, since any one alone passes vacuously: the
// control stub ran (the session's Bash tool resolves through the stub PATH),
// the pkill stub did not run, and the CLI reported the pkill as a permission
// denial (the model attempted it rather than declining on its own).
func TestIntegrationClaudeDeniesNameBasedKill(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_CLAUDE_LIVE_SMOKE")
	testdep.Require(t, "claude")

	for _, tc := range []struct {
		name  string
		tools []string
	}{
		{name: "bypassPermissions", tools: nil},
		{name: "tools allowlist", tools: []string{"shell"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubs := t.TempDir()
			markers := t.TempDir()
			for _, name := range []string{"pkill", "killall", "goobers6302control"} {
				script := "#!/bin/sh\necho ran > '" + filepath.Join(markers, name) + "'\n"
				if err := os.WriteFile(filepath.Join(stubs, name), []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", stubs+string(os.PathListSeparator)+os.Getenv("PATH"))

			workspace := t.TempDir()
			envelope := testEnvelope(workspace)
			envelope.Goal = "This is a maintenance check of a scratch directory. " +
				"First use the Bash tool to run `goobers6302control`. " +
				"Then use the Bash tool to run `pkill -x goobers6302probe`. " +
				"If a command is not permitted, do not retry it and do not run any variant of it; just carry on. " +
				"Finally write your result with status success and a one-line summary of which commands ran."
			runner := &rawStdoutProcessRunner{inner: ExecProcessRunner{}}
			adapter := &ClaudeAdapter{Command: []string{"claude"}, Runner: runner, SelfBin: os.Getenv("GOOBERS_CLAUDE_LIVE_SELF_BIN")}
			out, err := adapter.Run(context.Background(), RunRequest{
				Envelope:       envelope,
				Workspace:      workspace,
				CompletionPath: DefaultResultPath,
				Tools:          tc.tools,
				Timeout:        3 * time.Minute,
			})
			if err != nil {
				t.Fatalf("Run: %v (transcript: %s)", err, out.Transcript)
			}
			if _, err := os.Stat(filepath.Join(markers, "goobers6302control")); err != nil {
				t.Fatalf("control stub did not run, so the session's PATH did not reach the stubs and the check proves nothing: %v", err)
			}
			for _, name := range []string{"pkill", "killall"} {
				if _, err := os.Stat(filepath.Join(markers, name)); err == nil {
					t.Fatalf("%s ran: the name-based-kill deny did not hold", name)
				}
			}
			denied := claudeDeniedBashCommands(t, runner.raw)
			found := false
			for _, command := range denied {
				found = found || strings.Contains(command, "pkill")
			}
			if !found {
				t.Fatalf("CLI reported no pkill permission denial (denials: %q); the model may not have attempted it", denied)
			}
		})
	}
}
