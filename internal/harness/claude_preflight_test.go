package harness

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestClaudePreflightUsesConfiguredCredentialOnlyForAuthentication(t *testing.T) {
	for _, tc := range []struct{ name, token, want string }{
		{"oauth", "sk-ant-oat01-configured", "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-configured"},
		{"api-key", "sk-ant-api03-configured", "ANTHROPIC_API_KEY=sk-ant-api03-configured"},
		{"shared-copilot-grant", "github_pat_shared", "ANTHROPIC_API_KEY=ambient"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ANTHROPIC_API_KEY", "ambient")
			t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "ambient-oauth")
			t.Setenv("ANTHROPIC_AUTH_TOKEN", "ambient-bearer")
			runner := &claudeSequenceRunner{results: []ProcessResult{
				{Transcript: []byte("Claude Code test version")}, {Transcript: []byte(`{"loggedIn":true}`)},
			}}
			calls := 0
			adapter := &ClaudeAdapter{Command: []string{"echo"}, Runner: runner,
				ExtraEnvAllowlist: []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN"},
				ModelCredential:   func(context.Context) (string, error) { calls++; return tc.token, nil },
			}
			if _, err := adapter.Preflight(context.Background()); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || len(runner.reqs) != 2 {
				t.Fatalf("resolution calls=%d, process calls=%d", calls, len(runner.reqs))
			}
			auth := runner.reqs[1]
			if !slices.Contains(auth.Env, tc.want) {
				t.Fatalf("auth environment missing %q", tc.want)
			}
			if tc.name != "shared-copilot-grant" {
				for _, entry := range auth.Env {
					if strings.HasPrefix(entry, "ANTHROPIC_") || strings.HasPrefix(entry, "CLAUDE_CODE_OAUTH_TOKEN=") {
						if entry != tc.want {
							t.Fatalf("competing auth environment entry: %q", entry)
						}
					}
				}
			}
			for _, entry := range runner.reqs[0].Env {
				if strings.Contains(entry, tc.token) {
					t.Fatal("configured credential reached version probe")
				}
			}
			for _, arg := range auth.Command {
				if strings.Contains(arg, tc.token) {
					t.Fatal("credential reached argv")
				}
			}
		})
	}
}

func TestClaudePreflightFailsOnCredentialResolutionError(t *testing.T) {
	want := errors.New("credential store unavailable")
	runner := &claudeSequenceRunner{results: []ProcessResult{{Transcript: []byte("Claude Code test version")}}}
	adapter := &ClaudeAdapter{Command: []string{"echo"}, Runner: runner, ModelCredential: func(context.Context) (string, error) { return "", want }}
	if _, err := adapter.Preflight(context.Background()); !errors.Is(err, want) {
		t.Fatalf("error=%v, want credential resolution error", err)
	}
	if len(runner.reqs) != 1 {
		t.Fatal("auth probe ran after credential resolution failed")
	}
}
