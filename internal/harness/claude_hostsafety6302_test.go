package harness

import (
	"context"
	"slices"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// wantClaudeHostSafetyArgs is spelled out literally rather than derived from
// claudeDeniedNameKillRules, so dropping or narrowing a rule fails a test
// instead of silently shrinking the guard (#6302).
var wantClaudeHostSafetyArgs = []string{
	"--disallowedTools",
	"Bash(pkill:*)",
	"Bash(killall:*)",
	"Bash(/usr/bin/pkill:*)",
	"Bash(/usr/bin/killall:*)",
	"Bash(/bin/pkill:*)",
	"Bash(/bin/killall:*)",
	"Bash(/usr/bin/env pkill:*)",
	"Bash(/usr/bin/env killall:*)",
}

func TestClaudeHostSafetyArgsPinned(t *testing.T) {
	if got := claudeHostSafetyArgs(); !slices.Equal(got, wantClaudeHostSafetyArgs) {
		t.Fatalf("claudeHostSafetyArgs() = %v, want %v", got, wantClaudeHostSafetyArgs)
	}
	// The returned slice must be a fresh copy: a caller appending to it must
	// not be able to grow or rewrite the package-level rule list.
	first := claudeHostSafetyArgs()
	first[1] = "mutated"
	if got := claudeHostSafetyArgs(); !slices.Equal(got, wantClaudeHostSafetyArgs) {
		t.Fatalf("claudeHostSafetyArgs() aliased its rule list: %v", got)
	}
}

// containsRun reports whether want appears in command as one contiguous run,
// which is how a variadic --disallowedTools must reach the CLI: every rule a
// value of the same flag occurrence, none split off as a stray operand.
func containsRun(command, want []string) bool {
	for i := range command {
		if i+len(want) <= len(command) && slices.Equal(command[i:i+len(want)], want) {
			return true
		}
	}
	return false
}

// TestClaudeAdapterAlwaysDeniesNameBasedKill pins #6302: every claude-code
// invocation shape carries the name-based-kill deny, and an operator's own
// --settings / --disallowedTools from runner.harnessCommand reach the CLI
// unchanged beside it (the CLI unions rule sources, so neither side drops the
// other).
func TestClaudeAdapterAlwaysDeniesNameBasedKill(t *testing.T) {
	const operatorSettings = `{"permissions":{"deny":["Bash(rm -rf /:*)"],"allow":["Bash(pkill:*)"]}}`
	for _, tc := range []struct {
		name      string
		command   []string
		extraArgs []string
		tools     []string
		wantKept  [][]string
	}{
		{name: "bypassPermissions path", command: []string{"claude"}},
		{name: "tools allowlist path", command: []string{"claude"}, tools: []string{"shell"}},
		{name: "ExtraArgs override", command: []string{"claude"}, extraArgs: []string{"--output-format", "stream-json"}},
		{
			name: "operator harnessCommand settings",
			command: []string{
				"claude", "--settings", operatorSettings,
				"--disallowedTools", "Bash(shutdown:*)",
			},
			wantKept: [][]string{
				{"--settings", operatorSettings},
				{"--disallowedTools", "Bash(shutdown:*)"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubClaudeCredentialsHome(t)
			workspace := t.TempDir()
			runner := &fakeProcessRunner{
				result: ProcessResult{ExitCode: 0},
				act: func(req ProcessRequest) error {
					return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
				},
			}
			adapter := &ClaudeAdapter{Command: tc.command, ExtraArgs: tc.extraArgs, Runner: runner, KeychainReader: absentClaudeKeychain}
			if _, err := adapter.Run(context.Background(), RunRequest{
				Envelope:       testEnvelope(workspace),
				Workspace:      workspace,
				CompletionPath: DefaultResultPath,
				Tools:          tc.tools,
			}); err != nil {
				t.Fatalf("Run: %v", err)
			}
			command := runner.lastReq.Command
			if !containsRun(command, wantClaudeHostSafetyArgs) {
				t.Fatalf("command lacks the name-based-kill deny %v: %v", wantClaudeHostSafetyArgs, command)
			}
			for _, kept := range tc.wantKept {
				if !containsRun(command, kept) {
					t.Errorf("operator arguments %v were not passed through unchanged: %v", kept, command)
				}
			}
		})
	}
}

// TestClaudeAdapterCompletionRepairKeepsNameBasedKillDeny covers the second
// process of a run: the same-session completion repair resumes from a copy of
// the first argv and must not shed the deny on the way.
func TestClaudeAdapterCompletionRepairKeepsNameBasedKillDeny(t *testing.T) {
	stubClaudeCredentialsHome(t)
	workspace := t.TempDir()
	runner := &claudeSequenceRunner{
		results: []ProcessResult{
			{ExitCode: 0, Transcript: []byte(claudeResultStream)},
			{ExitCode: 0, Transcript: []byte(claudeResultStream)},
		},
		acts: []func(ProcessRequest) error{
			nil,
			func(req ProcessRequest) error {
				return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
			},
		},
	}
	adapter := &ClaudeAdapter{Command: []string{"claude"}, Runner: runner, KeychainReader: absentClaudeKeychain}
	if _, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Timeout:        time.Minute,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(runner.reqs) != 2 {
		t.Fatalf("process calls = %d, want 2", len(runner.reqs))
	}
	for i, req := range runner.reqs {
		if !containsRun(req.Command, wantClaudeHostSafetyArgs) {
			t.Fatalf("process %d lacks the name-based-kill deny: %v", i+1, req.Command)
		}
	}
	if commandOptionValue(runner.reqs[1].Command, "--resume") == "" {
		t.Fatalf("second process is not the completion repair: %v", runner.reqs[1].Command)
	}
}

// TestClaudeAdapterSandboxedRunKeepsNameBasedKillDeny: confinement wraps the
// argv rather than rebuilding it, so the deny survives as defense in depth
// alongside the native sandbox's process restrictions.
func TestClaudeAdapterSandboxedRunKeepsNameBasedKillDeny(t *testing.T) {
	stubClaudeCredentialsHome(t)
	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0, Transcript: []byte(claudeResultStream)},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &ClaudeAdapter{Command: []string{"claude"}, Runner: runner, KeychainReader: absentClaudeKeychain}
	if _, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Sandbox:        &stubSandbox{},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	command := runner.lastReq.Command
	if len(command) == 0 || command[0] != "sandbox-stub" {
		t.Fatalf("command is not sandbox-wrapped: %v", command)
	}
	if !containsRun(command, wantClaudeHostSafetyArgs) {
		t.Fatalf("sandboxed command lacks the name-based-kill deny: %v", command)
	}
}
