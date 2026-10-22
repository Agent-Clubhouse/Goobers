package harness

import (
	"context"
	"errors"
	"os"
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
		version string
	}{
		{name: "built-in default", command: []string{"copilot"}},
		{name: "launcher override without flag", command: []string{"forwarding-launcher", "--profile", "fixture"}},
		{name: "override already carries flag", command: []string{"copilot", copilotNoRemoteExportFlag}},
		{name: "extra args already carry flag", command: []string{"copilot"}, extra: []string{"--allow-all-tools", copilotNoRemoteExportFlag}},
		{name: "operator requests export", command: []string{"copilot", "--remote-export"}},
		{name: "modern CLI replaces legacy override", command: []string{"copilot", copilotNoRemoteExportFlag}, version: "GitHub Copilot CLI 1.0.103."},
		{name: "modern CLI disables configured sync", command: []string{"copilot", "--sync-session=on"}, extra: []string{"--remote-export"}, version: "1.0.103"},
		{name: "legacy CLI replaces sync override", command: []string{"copilot", "--sync-session", "on"}, version: "1.0.102-0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeProcessRunner{
				act: func(req ProcessRequest) error {
					return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
				},
			}
			adapter := &CopilotAdapter{Command: tc.command, ExtraArgs: tc.extra, Runner: runner}
			if _, err := adapter.Run(context.Background(), RunRequest{Workspace: t.TempDir(), CompletionPath: DefaultResultPath, HarnessVersion: tc.version}); err != nil {
				t.Fatalf("Run: %v", err)
			}
			argv := runner.lastReq.Command
			flag := copilotSessionExportFlag(tc.version)
			if got := countArg(argv, flag); got != 1 {
				t.Fatalf("argv carries %s %d times, want exactly once: %q", flag, got, argv)
			}
			for _, arg := range argv {
				if arg != flag && (arg == copilotNoRemoteExportFlag || arg == "--remote-export" || strings.HasPrefix(arg, "--sync-session")) {
					t.Fatalf("argv retains incompatible export control %q: %q", arg, argv)
				}
			}
		})
	}
}

// TestCopilotPreflightFallbackAuthProbeDisablesRemoteExport covers the sign-in
// probe, which is itself a real prompt session.
func TestCopilotPreflightFallbackAuthProbeDisablesRemoteExport(t *testing.T) {
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		authCheck []string
		version   string
	}{
		{name: "fallback prompt", authCheck: []string{"-p", "Reply with exactly: ok", "--allow-all-tools", "--available-tools="}, version: "1.0.83"},
		{name: "already present", authCheck: []string{"-p", "ok", copilotNoRemoteExportFlag}, version: "1.0.83"},
		{name: "modern fallback prompt", authCheck: []string{"-p", "ok", copilotNoRemoteExportFlag}, version: "GitHub Copilot CLI 1.0.103."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var authCommand []string
			runner := &fakeProcessRunner{
				result: ProcessResult{Transcript: []byte(tc.version + "\n")},
				act: func(req ProcessRequest) error {
					if slices.Contains(req.Command, "-p") {
						authCommand = slices.Clone(req.Command)
					}
					return nil
				},
			}
			adapter := &CopilotAdapter{Command: []string{program}, AuthCheckArgs: tc.authCheck, Runner: runner}
			if _, err := adapter.Preflight(context.Background()); err != nil {
				t.Fatalf("Preflight: %v", err)
			}
			flag := copilotSessionExportFlag(tc.version)
			if got := countArg(authCommand, flag); got != 1 {
				t.Fatalf("auth probe carries %s %d times, want exactly once: %q", flag, got, authCommand)
			}
		})
	}
}

func TestCopilotSessionExportFlag(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    string
	}{
		{"", copilotNoRemoteExportFlag},
		{"wrapper unknown version", copilotNoRemoteExportFlag},
		{"1.0.52", copilotNoRemoteExportFlag},
		{"GitHub Copilot CLI 1.0.102-0.", copilotNoRemoteExportFlag},
		{"1.0.103-0", copilotNoRemoteExportFlag},
		{"GitHub Copilot CLI 1.0.103.", copilotSyncSessionOffFlag},
		{"copilot version 1.0.104", copilotSyncSessionOffFlag},
		{"v2.0.0", copilotSyncSessionOffFlag},
	} {
		t.Run(tc.version, func(t *testing.T) {
			if got := copilotSessionExportFlag(tc.version); got != tc.want {
				t.Fatalf("flag = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCopilotSessionExportControlsAreReplaced(t *testing.T) {
	for _, reported := range []string{"1.0.102", "1.0.103"} {
		for _, controls := range [][]string{
			{"--sync-session"},
			{"--sync-session", "on"},
			{"--sync-session", "off"},
			{"--sync-session=on", "--sync-session=off"},
			{copilotNoRemoteExportFlag, "--remote-export"},
		} {
			argv := append([]string{"copilot"}, controls...)
			argv = append(argv, "--model", "fixture")
			original := slices.Clone(argv)
			want := []string{"copilot", "--model", "fixture", copilotSessionExportFlag(reported)}
			got := withCopilotNoRemoteExport(argv, reported)
			if !slices.Equal(got, want) || !slices.Equal(argv, original) {
				t.Fatalf("version %s controls %q: got %q, want %q; original argv now %q", reported, controls, got, want, argv)
			}
			if twice := withCopilotNoRemoteExport(got, reported); !slices.Equal(twice, got) {
				t.Fatalf("not idempotent: %q became %q", got, twice)
			}
		}
	}
}

func TestCopilotModernExportFlagRejectionIsNotTooOld(t *testing.T) {
	for _, flag := range []string{"--sync-session", copilotSyncSessionOffFlag} {
		result := ProcessResult{Stderr: []byte("error: unknown option '" + flag + "'")}
		if !copilotRemoteExportUnsupported(result) {
			t.Fatalf("did not detect rejected %s", flag)
		}
	}
	err := copilotRemoteExportUnsupportedError("GitHub Copilot CLI 1.0.103.")
	if strings.Contains(err.Error(), "too old") || !strings.Contains(err.Error(), copilotSyncSessionOffFlag) {
		t.Fatalf("misleading error: %v", err)
	}
}

// TestCopilotPreflightNamesMinimumVersionWhenFlagUnknown: a CLI older than
// 1.0.52 rejects the flag as an unknown option. Preflight fails closed (those
// CLIs can still export sessions via user configuration, so dropping the flag
// is not privacy-equivalent) and says to upgrade rather than blame sign-in.
func TestCopilotPreflightNamesMinimumVersionWhenFlagUnknown(t *testing.T) {
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
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
	adapter := &CopilotAdapter{Command: []string{program}, AuthCheckArgs: []string{"-p", "ok"}, Runner: runner}
	_, err = adapter.Preflight(context.Background())
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
