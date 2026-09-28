package harness

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func countArg(argv []string, arg string) int {
	n := 0
	for _, a := range argv {
		if a == arg {
			n++
		}
	}
	return n
}

// TestCopilotRunEnforcesNoRemoteExportOnFinalArgv proves the export opt-out is
// on every stage session's final argv whatever the launcher prefix or
// ExtraArgs, and is never duplicated when an operator already passes it.
func TestCopilotRunEnforcesNoRemoteExportOnFinalArgv(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command []string
		extra   []string
	}{
		{name: "built-in default", command: []string{"copilot"}},
		{name: "launcher override without flag", command: []string{"forwarding-launcher", "--profile", "fixture"}},
		{name: "override already carries flag", command: []string{"copilot", copilotNoRemoteExportFlag}},
		{name: "extra args already carry flag", command: []string{"copilot"}, extra: []string{"--allow-all-tools", copilotNoRemoteExportFlag}},
		{name: "operator requests export", command: []string{"copilot", "--remote-export"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeProcessRunner{
				act: func(req ProcessRequest) error {
					return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
				},
			}
			adapter := &CopilotAdapter{Command: tc.command, ExtraArgs: tc.extra, Runner: runner}
			if _, err := adapter.Run(context.Background(), RunRequest{Workspace: t.TempDir(), CompletionPath: DefaultResultPath}); err != nil {
				t.Fatalf("Run: %v", err)
			}
			argv := runner.lastReq.Command
			if got := countArg(argv, copilotNoRemoteExportFlag); got != 1 {
				t.Fatalf("argv carries %s %d times, want exactly once: %q", copilotNoRemoteExportFlag, got, argv)
			}
			// Appended after any operator-supplied --remote-export, so the
			// opt-out is the one the CLI honors.
			if i := slices.Index(argv, "--remote-export"); i >= 0 && i > slices.Index(argv, copilotNoRemoteExportFlag) {
				t.Fatalf("--remote-export follows the opt-out and would win: %q", argv)
			}
		})
	}
}

// TestCopilotPreflightFallbackAuthProbeDisablesRemoteExport covers the sign-in
// probe, which is itself a real prompt session.
func TestCopilotPreflightFallbackAuthProbeDisablesRemoteExport(t *testing.T) {
	for _, tc := range []struct {
		name      string
		authCheck []string
	}{
		{name: "fallback prompt", authCheck: []string{"-p", "Reply with exactly: ok", "--allow-all-tools", "--available-tools="}},
		{name: "already present", authCheck: []string{"-p", "ok", copilotNoRemoteExportFlag}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var authCommand []string
			runner := &fakeProcessRunner{
				result: ProcessResult{Transcript: []byte("GitHub Copilot CLI 1.0.83.\n")},
				act: func(req ProcessRequest) error {
					if slices.Contains(req.Command, "-p") {
						authCommand = slices.Clone(req.Command)
					}
					return nil
				},
			}
			adapter := &CopilotAdapter{Command: []string{"echo"}, AuthCheckArgs: tc.authCheck, Runner: runner}
			if _, err := adapter.Preflight(context.Background()); err != nil {
				t.Fatalf("Preflight: %v", err)
			}
			if got := countArg(authCommand, copilotNoRemoteExportFlag); got != 1 {
				t.Fatalf("auth probe carries %s %d times, want exactly once: %q", copilotNoRemoteExportFlag, got, authCommand)
			}
		})
	}
}

// TestCopilotPreflightNamesMinimumVersionWhenFlagUnknown: a CLI older than
// 1.0.52 rejects the flag as an unknown option. Preflight fails closed (those
// CLIs can still export sessions via user configuration, so dropping the flag
// is not privacy-equivalent) and says to upgrade rather than blame sign-in.
func TestCopilotPreflightNamesMinimumVersionWhenFlagUnknown(t *testing.T) {
	runner := &fakeProcessRunner{
		result: ProcessResult{Transcript: []byte("GitHub Copilot CLI 1.0.51.\n")},
	}
	runner.act = func(req ProcessRequest) error {
		if slices.Contains(req.Command, copilotNoRemoteExportFlag) {
			runner.result = ProcessResult{
				ExitCode:   1,
				Transcript: []byte("error: unknown option '--no-remote-export'\n\nTry 'copilot --help' for more information.\n"),
			}
			return errors.New("exit status 1")
		}
		return nil
	}
	adapter := &CopilotAdapter{Command: []string{"echo"}, AuthCheckArgs: []string{"-p", "ok"}, Runner: runner}
	_, err := adapter.Preflight(context.Background())
	if err == nil {
		t.Fatal("expected preflight to fail when the CLI rejects the export opt-out")
	}
	for _, want := range []string{"GitHub Copilot CLI 1.0.51.", copilotNoRemoteExportMinVersion, copilotNoRemoteExportFlag, "upgrade"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "sign in") {
		t.Fatalf("error should name the minimum Copilot CLI version, not blame sign-in: %v", err)
	}
}
