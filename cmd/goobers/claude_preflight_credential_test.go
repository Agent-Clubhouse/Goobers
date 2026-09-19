package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/secretstore"
)

type claudeCredentialProbeRunner struct{ authEnv []string }

func (r *claudeCredentialProbeRunner) Run(_ context.Context, req harness.ProcessRequest) (harness.ProcessResult, error) {
	if slices.Contains(req.Command, "auth") {
		r.authEnv = slices.Clone(req.Env)
	}
	return harness.ProcessResult{Transcript: []byte("Claude Code test version")}, nil
}

func TestClaudePreflightRegistryUsesScopedFileCredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude-token")
	const token = "sk-ant-oat01-scoped-file"
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &instance.Config{Credentials: []instance.CredentialGrant{
		{Capability: "agent:model", Token: instance.TokenRef{Env: "UNUSED_COPILOT_TOKEN"}},
		{Capability: "agent:model", Harness: string(apiv1.HarnessClaudeCode), Token: instance.TokenRef{File: path}},
	}}
	stores, err := secretstore.NewRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	resolve, err := harnessModelCredentialResolver(cfg, stores)(apiv1.HarnessClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := adapterFor(apiv1.HarnessClaudeCode, harness.EnvironmentConfig{}, map[string][]string{"claude-code": {"echo"}}, resolve)
	if err != nil {
		t.Fatal(err)
	}
	runner := &claudeCredentialProbeRunner{}
	adapter.(*harness.ClaudeAdapter).Runner = runner
	if _, err := adapter.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(runner.authEnv, "CLAUDE_CODE_OAUTH_TOKEN="+token) {
		t.Fatal("scoped file credential did not reach the real Claude adapter's auth probe")
	}
}
