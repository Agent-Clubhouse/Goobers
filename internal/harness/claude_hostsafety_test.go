package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestClaudeRefusesHookBypassOptionsBeforeDispatch(t *testing.T) {
	for _, args := range [][]string{
		{"--bare"}, {"--safe-mode"}, {"--restricted"},
		{"CLAUDE_CODE_SIMPLE=1"},
		{"--setting-sources", ""}, {"--setting-sources=project,local"}, {"--setting-sources=user,project"},
		{"--settings", "custom-settings.json"}, {"--settings={\"disableAllHooks\":true}"},
		{"--settings", `{"hooks":{}}`}, {"--settings", `{"allowManagedHooksOnly":true}`},
		{"--settings", `{"env":{"CLAUDE_CODE_SIMPLE":"1"}}`}, {"--settings", "null"}, {"--settings"},
	} {
		for _, inCommand := range []bool{false, true} {
			adapter := &ClaudeAdapter{Command: []string{"claude"}, ExtraArgs: args, Runner: &fakeProcessRunner{act: func(ProcessRequest) error {
				t.Fatal("unsafe configuration dispatched")
				return nil
			}}}
			if inCommand {
				adapter.Command = append(adapter.Command, args...)
				adapter.ExtraArgs = nil
			}
			_, err := adapter.Run(context.Background(), RunRequest{Workspace: t.TempDir()})
			if err == nil || !strings.Contains(err.Error(), "harness: claude-code:") {
				t.Fatalf("options %q were not refused: %v", args, err)
			}
		}
	}
}

func TestClaudeHostSafetySettingsSurviveBothPermissionModesAndRepair(t *testing.T) {
	for _, tools := range [][]string{nil, {"shell"}} {
		stubClaudeCredentialsHome(t)
		workspace := t.TempDir()
		var calls int
		runner := &fakeProcessRunner{act: func(req ProcessRequest) error {
			calls++
			var config string
			for _, entry := range req.Env {
				if value, ok := strings.CutPrefix(entry, "CLAUDE_CONFIG_DIR="); ok {
					config = value
				}
			}
			data, err := os.ReadFile(filepath.Join(config, "settings.json"))
			if err != nil {
				t.Fatal(err)
			}
			var settings struct {
				Hooks map[string][]struct {
					Matcher string
					Hooks   []struct {
						Type, Command, OnFailure string
						Args                     []string
						Timeout                  int
					}
				}
			}
			if err := json.Unmarshal(data, &settings); err != nil {
				t.Fatal(err)
			}
			groups := settings.Hooks["PreToolUse"]
			if len(groups) != 1 || groups[0].Matcher != "Bash" || len(groups[0].Hooks) != 1 {
				t.Fatalf("missing Bash hook: %s", data)
			}
			hook := groups[0].Hooks[0]
			if !filepath.IsAbs(hook.Command) || hook.Type != "command" || hook.OnFailure != "block" ||
				!slices.Equal(hook.Args, []string{"__claude-host-safety"}) || hook.Timeout != 10 {
				t.Fatalf("hook is not a bounded exec-form fail-closed command: %+v", hook)
			}
			if !containsRun(req.Command, []string{"--setting-sources", "user"}) {
				t.Fatal("invocation can load project/local hook overrides")
			}
			if calls == 1 {
				return nil // Missing completion forces the same-session repair.
			}
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		}}
		adapter := &ClaudeAdapter{Command: []string{"claude", "--settings", `{"permissions":{"allow":["Bash(pkill:*)"]}}`}, Runner: runner, KeychainReader: absentClaudeKeychain}
		if _, err := adapter.Run(context.Background(), RunRequest{Workspace: workspace, Tools: tools, CompletionPath: DefaultResultPath, HarnessVersion: "2.1.302 (Claude Code)"}); err != nil {
			t.Fatal(err)
		}
		if calls != 2 {
			t.Fatalf("calls=%d, want initial turn and repair", calls)
		}
	}
}

func TestClaudeHostSafetyVersionFloor(t *testing.T) {
	for _, version := range []string{"2.1.292 (Claude Code)", "2.1.301 (Claude Code)", "unknown"} {
		if validateClaudeHostSafetyVersion(version) == nil {
			t.Fatalf("unsupported version accepted: %s", version)
		}
	}
	for _, version := range []string{"2.1.302 (Claude Code)", "2.2.0", "3.0.0"} {
		if err := validateClaudeHostSafetyVersion(version); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{
		"CLAUDE_CODE_SIMPLE",
		"CLAUDE_CODE_SAFE_MODE",
		"CLAUDE_CODE_DISABLE_HOOKS",
		"claude_code_disable_hooks",
		"Claude_Code_Safe_Mode",
	} {
		if validateClaudeHostSafetyEnv([]string{key + "=1"}) == nil {
			t.Fatalf("%s bypass accepted", key)
		}
		if err := validateClaudeHostSafetyEnv([]string{key + "=0"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestClaudeHookCompatibilityAndRequiredPolicy(t *testing.T) {
	for _, version := range []string{"2.1.292 (Claude Code)", "unknown", "2.1.302 (Claude Code)"} {
		for _, required := range []bool{false, true} {
			options := testHarnessOptions(t, map[string]interface{}{"requireHostProcessHook": required})
			supported := version == "2.1.302 (Claude Code)"
			runner := &claudeSequenceRunner{results: []ProcessResult{
				{Transcript: []byte(version)}, {Transcript: []byte(`{"loggedIn":true}`)},
			}}
			adapter := &ClaudeAdapter{Command: []string{"echo"}, Runner: runner}
			info, err := adapter.PreflightConfig(context.Background(), "", options)
			if required && !supported {
				if err == nil || !strings.Contains(err.Error(), "upgrade") {
					t.Fatalf("required hook admitted unsupported version %q: %v", version, err)
				}
			} else if err != nil || (len(info.Warnings) > 0) == supported {
				t.Fatalf("preflight %q required=%v: info=%+v err=%v", version, required, info, err)
			}

			stubClaudeCredentialsHome(t)
			dispatched := false
			adapter.Runner = &fakeProcessRunner{act: func(req ProcessRequest) error {
				dispatched = true
				var config string
				for _, entry := range req.Env {
					if value, ok := strings.CutPrefix(entry, "CLAUDE_CONFIG_DIR="); ok {
						config = value
					}
				}
				_, err := os.Stat(filepath.Join(config, "settings.json"))
				if supported != (err == nil) {
					t.Fatalf("hook installation does not match CLI support: %v", err)
				}
				if !containsRun(req.Command, wantClaudeHostSafetyArgs) {
					t.Fatal("compatibility invocation lost existing CLI denies")
				}
				return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
			}}
			adapter.KeychainReader = absentClaudeKeychain
			_, err = adapter.Run(context.Background(), RunRequest{
				Workspace: t.TempDir(), CompletionPath: DefaultResultPath,
				HarnessVersion: version, HarnessOptions: options,
			})
			if required && !supported {
				if err == nil || dispatched {
					t.Fatal("required hook dispatched unsupported CLI")
				}
			} else if err != nil || !dispatched {
				t.Fatalf("compatible invocation failed: %v", err)
			}
		}
	}
}
