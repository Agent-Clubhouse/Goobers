package harness

import (
	"context"
	"slices"
	"testing"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestCopilotAlwaysCarriesHostSafetyDenies(t *testing.T) {
	for _, constrained := range []bool{false, true} {
		for _, command := range [][]string{{"copilot"}, {"launcher", "--profile", "fixture"}} {
			var tools []string
			if constrained {
				tools = []string{"shell"}
			}
			runner := &fakeProcessRunner{act: func(req ProcessRequest) error {
				if constrained {
					_, err := req.StdoutCapture.Write([]byte(`{"status":"success","summary":"done"}`))
					return err
				}
				return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
			}}
			adapter := &CopilotAdapter{Command: command, ExtraArgs: []string{"--allow-all-tools", "--allow-tool=shell(pkill)"}, Runner: runner}
			_, err := adapter.Run(context.Background(), RunRequest{Workspace: t.TempDir(), Tools: tools, CompletionPath: DefaultResultPath})
			if err != nil {
				t.Fatal(err)
			}
			argv := runner.lastReq.Command
			for _, name := range []string{"pkill", "killall", "/usr/bin/pkill", "/usr/bin/killall", "/bin/pkill", "/bin/killall", "/usr/bin/env pkill", "/usr/bin/env killall"} {
				flag := "--deny-tool=shell(" + name + ")"
				if !slices.Contains(argv, flag) {
					t.Fatalf("missing %s: %v", flag, argv)
				}
				// The headless conversion must retain the same denies.
				headless, _, err := copilotControlCommand(argv, len(command))
				if err != nil || !slices.Contains(headless, flag) {
					t.Fatalf("headless conversion dropped deny: %v, %v", headless, err)
				}
				if !slices.Contains(copilotPermissionArgs(argv), flag) {
					t.Fatalf("diagnostics omitted %s", flag)
				}
			}
		}
	}
}

func TestCopilotControlledSessionRejectsWrappedNameKills(t *testing.T) {
	workspace := t.TempDir()
	handler := copilotSessionPermissions(RunRequest{Workspace: workspace, Tools: []string{"shell"}}, nil)
	for _, command := range []string{
		"pkill -f cat", "killall cat", "/custom/bin/pkill cat",
		"bash -c 'pkill -f cat'", `sh -c "killall cat"`, "eval pkill cat",
		"pgrep -f cat | xargs -I{} kill {}", "pgrep -f cat | xargs -I{} pkill {}",
		`find . -exec pkill cat \;`, "$(command -v pkill) cat", "cd /usr/bin && ./pkill cat",
		"kill $(pgrep -f cat)",
	} {
		t.Run(command, func(t *testing.T) {
			decision, err := handler(rpc.PermissionRequestShell{FullCommandText: command}, copilot.PermissionInvocation{})
			if _, approved := decision.(*rpc.PermissionDecisionApproveOnce); err != nil || approved {
				t.Fatalf("unsafe command approved: %T, %v", decision, err)
			}
		})
	}
	for _, command := range []string{"git status", "go test ./...", "kill -TERM 12345", "pgrep -f fixture", "echo pkill_count"} {
		decision, err := handler(rpc.PermissionRequestShell{FullCommandText: command}, copilot.PermissionInvocation{})
		if _, approved := decision.(*rpc.PermissionDecisionApproveOnce); err != nil || !approved {
			t.Fatalf("safe command %q refused: %T, %v", command, decision, err)
		}
	}
}
