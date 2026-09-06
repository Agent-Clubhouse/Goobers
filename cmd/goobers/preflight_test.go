package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/secretstore"
)

// TestAgentModelCredentialResolverResolvesFileRef is the #3341 control: a
// file-sourced agent:model credential (instance.yaml's `credentials:
// [{capability: agent:model, token: {file: ...}}]`) must be resolvable by
// name, the same shape the daemon-startup preflight now hands to a harness's
// Preflight sign-in probe.
func TestAgentModelCredentialResolverResolvesFileRef(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "copilot.pat")
	if err := os.WriteFile(tokenPath, []byte("pat-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &instance.Config{
		Credentials: []instance.CredentialGrant{{
			Capability: string(capability.AgentModel),
			Token:      instance.TokenRef{File: tokenPath},
		}},
	}
	stores, err := secretstore.NewRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	resolve, err := agentModelCredentialResolver(cfg, stores)
	if err != nil {
		t.Fatalf("agentModelCredentialResolver: %v", err)
	}
	if resolve == nil {
		t.Fatal("expected a non-nil resolver for a configured agent:model grant")
	}
	got, err := resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "pat-from-file" {
		t.Fatalf("resolve = %q, want %q", got, "pat-from-file")
	}
}

// TestAgentModelCredentialResolverNilWithoutGrant confirms the resolver is
// nil (not an error) when the instance has no agent:model credential
// configured — preflight then falls back to ambient env or the CLI's own
// cached login, unchanged from before this resolver existed.
func TestAgentModelCredentialResolverNilWithoutGrant(t *testing.T) {
	cfg := &instance.Config{}
	stores, err := secretstore.NewRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	resolve, err := agentModelCredentialResolver(cfg, stores)
	if err != nil {
		t.Fatalf("agentModelCredentialResolver: %v", err)
	}
	if resolve != nil {
		t.Fatal("expected a nil resolver when no agent:model grant is configured")
	}
}

// harnessFakeRunner is a ProcessRunner double reporting a fixed exit code, used
// to drive a real CopilotAdapter's Preflight to success/failure without a real
// CLI subprocess.
type harnessFakeRunner struct{ exit int }

func (r *harnessFakeRunner) Run(context.Context, harness.ProcessRequest) (harness.ProcessResult, error) {
	return harness.ProcessResult{ExitCode: r.exit, Transcript: []byte("copilot version 1.2.3\n")}, nil
}

// TestPreflightAgenticHarnesses is the #238 control: an agentic stage's unusable
// harness fails preflight (fail closed), a healthy one passes, and a
// deterministic-only workflow preflights no harness at all.
func TestPreflightAgenticHarnesses(t *testing.T) {
	orig := harnessAdapterFor
	t.Cleanup(func() { harnessAdapterFor = orig })

	goobers := map[string]apiv1.GooberSpec{"nominator": {Harness: apiv1.HarnessCopilot}}
	agentic := []apiv1.Workflow{{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{
		{Name: "nominate", Type: apiv1.TaskAgentic, Goober: "nominator"},
	}}}}
	deterministicOnly := []apiv1.Workflow{{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{
		{Name: "gather", Type: apiv1.TaskDeterministic},
	}}}}

	// Unusable harness (its version check exits non-zero) → fail closed.
	harnessAdapterFor = func(apiv1.Harness, []string, map[string][]string, func(context.Context) (string, error)) (harness.Adapter, error) {
		return &harness.CopilotAdapter{Command: []string{"echo"}, Runner: &harnessFakeRunner{exit: 1}}, nil
	}
	if _, err := preflightAgenticHarnesses(goobers, agentic, nil, nil, nil); err == nil {
		t.Fatal("expected preflight to fail closed on an unusable agentic harness")
	}
	// A deterministic-only workflow references no harness, so it must not be
	// gated by a broken harness (the adapter would fail if consulted).
	if _, err := preflightAgenticHarnesses(goobers, deterministicOnly, nil, nil, nil); err != nil {
		t.Fatalf("deterministic-only workflow must not preflight a harness: %v", err)
	}

	// Healthy harness → preflight passes.
	var gotEnvPassthrough []string
	harnessAdapterFor = func(_ apiv1.Harness, envPassthrough []string, _ map[string][]string, _ func(context.Context) (string, error)) (harness.Adapter, error) {
		gotEnvPassthrough = append([]string(nil), envPassthrough...)
		return &harness.CopilotAdapter{Command: []string{"echo"}, Runner: &harnessFakeRunner{exit: 0}}, nil
	}
	info, err := preflightAgenticHarnesses(goobers, agentic, []string{"CLAUDE_CONFIG_DIR"}, nil, nil)
	if err != nil {
		t.Fatalf("healthy agentic harness should preflight OK: %v", err)
	}
	if got := info[apiv1.HarnessCopilot].Version; got != "copilot version 1.2.3" {
		t.Fatalf("preflight version = %q", got)
	}
	if strings.Join(gotEnvPassthrough, ",") != "CLAUDE_CONFIG_DIR" {
		t.Fatalf("adapter env passthrough = %v, want [CLAUDE_CONFIG_DIR]", gotEnvPassthrough)
	}

	gateOnly := []apiv1.Workflow{{Spec: apiv1.WorkflowSpec{Gates: []apiv1.Gate{{
		Name: "review", Evaluator: apiv1.EvaluatorAgentic,
		Agentic: &apiv1.AgenticGate{Goober: "reviewer"},
	}}}}}
	info, err = preflightAgenticHarnesses(
		map[string]apiv1.GooberSpec{"reviewer": {}},
		gateOnly,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("reviewer-only default harness preflight: %v", err)
	}
	if got := info[apiv1.HarnessCopilot].Version; got != "copilot version 1.2.3" {
		t.Fatalf("reviewer preflight version = %q", got)
	}
}

// TestAdapterForConfiguresAuthProbe proves the #238 wiring: the default
// CopilotAdapter carries the auth probe (copilotAuthCheckArgs), so every
// preflight through adapterFor — validate --check-harness AND the automatic
// daemon-startup preflight — verifies sign-in, not just CLI presence.
func TestAdapterForConfiguresAuthProbe(t *testing.T) {
	a, err := adapterFor(apiv1.HarnessCopilot, []string{"CLAUDE_CONFIG_DIR"}, nil, nil)
	if err != nil {
		t.Fatalf("adapterFor(copilot): %v", err)
	}
	ca, ok := a.(*harness.CopilotAdapter)
	if !ok {
		t.Fatalf("adapterFor returned %T, want *harness.CopilotAdapter", a)
	}
	if len(ca.AuthCheckArgs) == 0 {
		t.Fatal("adapterFor's CopilotAdapter has no AuthCheckArgs — the daemon-startup preflight would skip the sign-in probe (#238)")
	}
	if strings.Join(ca.AuthCheckArgs, " ") != strings.Join(copilotAuthCheckArgs, " ") {
		t.Fatalf("AuthCheckArgs = %v, want the confirmed probe %v", ca.AuthCheckArgs, copilotAuthCheckArgs)
	}
	if strings.Join(ca.ExtraEnvAllowlist, ",") != "CLAUDE_CONFIG_DIR" {
		t.Fatalf("ExtraEnvAllowlist = %v, want [CLAUDE_CONFIG_DIR]", ca.ExtraEnvAllowlist)
	}
}

// TestPreflightAgenticHarnessesCatchesSignedOut is #238 AC3: a harness that is
// installed (--version exits 0) but signed out (the auth probe exits non-zero)
// now fails the automatic daemon-startup preflight — the #284 incident caught
// at startup instead of as a burned mid-run agentic attempt. Before #238 the
// startup path ran only the version check, so this signed-out harness passed
// preflight and failed later, mid-run.
func TestPreflightAgenticHarnessesCatchesSignedOut(t *testing.T) {
	orig := harnessAdapterFor
	t.Cleanup(func() { harnessAdapterFor = orig })

	goobers := map[string]apiv1.GooberSpec{"nominator": {Harness: apiv1.HarnessCopilot}}
	agentic := []apiv1.Workflow{{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{
		{Name: "nominate", Type: apiv1.TaskAgentic, Goober: "nominator"},
	}}}}

	// Installed but signed out: version 0, auth probe non-zero. The adapter
	// carries copilotAuthCheckArgs (as the real adapterFor now does), so the
	// probe actually runs during the startup preflight.
	harnessAdapterFor = func(apiv1.Harness, []string, map[string][]string, func(context.Context) (string, error)) (harness.Adapter, error) {
		return &harness.CopilotAdapter{
			Command:       []string{"echo"},
			AuthCheckArgs: copilotAuthCheckArgs,
			Runner:        &authProbeFakeRunner{versionExit: 0, authExit: 1},
		}, nil
	}
	_, err := preflightAgenticHarnesses(goobers, agentic, nil, nil, nil)
	if err == nil {
		t.Fatal("expected the daemon-startup preflight to fail closed on a signed-out harness")
	}
	if !strings.Contains(err.Error(), "sign-in check") {
		t.Fatalf("err = %v, want it to mention the sign-in check (the auth probe, not the version check)", err)
	}
}

// fileRefPreflightRunner records the environment each probe subprocess would
// have received and reports success, so the test observes what the preflight
// authenticates WITH rather than requiring a real, signed-in Copilot CLI.
type fileRefPreflightRunner struct{ authProbeEnv []string }

func (r *fileRefPreflightRunner) Run(_ context.Context, req harness.ProcessRequest) (harness.ProcessResult, error) {
	for _, arg := range req.Command {
		if arg == "auth" {
			r.authProbeEnv = append([]string(nil), req.Env...)
		}
	}
	return harness.ProcessResult{ExitCode: 0, Transcript: []byte("copilot version 1.2.3\n")}, nil
}

// TestCopilotPreflightSatisfiedByFileRefOnlyCredential is #4292's acceptance
// criterion, joined end to end: an instance.yaml whose ONLY delivery of the
// agent:model token is a `credentials:` file ref — no COPILOT_GITHUB_TOKEN,
// GH_TOKEN or GITHUB_TOKEN anywhere in the process environment — must satisfy
// the Copilot sign-in preflight, and the probe must carry that credential.
//
// This is the shape the issue reports as broken: the production instance had to
// deliver the same token twice, once through the resolver and once as a plain
// pod env var outside every control the resolver provides, because the preflight
// read only the ambient environment. The two halves are each covered in their
// own package (agentModelCredentialResolver above; the adapter's precedence in
// internal/harness), but only their composition proves the configuration a
// stranger writes actually works — which is what was untrue.
func TestCopilotPreflightSatisfiedByFileRefOnlyCredential(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	for _, name := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}

	tokenPath := filepath.Join(t.TempDir(), "copilot.pat")
	if err := os.WriteFile(tokenPath, []byte("pat-from-file-ref-only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &instance.Config{
		Credentials: []instance.CredentialGrant{{
			Capability: string(capability.AgentModel),
			Token:      instance.TokenRef{File: tokenPath},
		}},
	}
	stores, err := secretstore.NewRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	resolve, err := agentModelCredentialResolver(cfg, stores)
	if err != nil {
		t.Fatalf("agentModelCredentialResolver: %v", err)
	}
	if resolve == nil {
		t.Fatal("expected a resolver for the configured agent:model grant")
	}

	runner := &fileRefPreflightRunner{}
	adapter := &harness.CopilotAdapter{
		Command:         []string{"echo"},
		AuthCheckArgs:   []string{"auth", "status"},
		Runner:          runner,
		ModelCredential: resolve,
	}
	info, err := adapter.Preflight(context.Background())
	if err != nil {
		t.Fatalf("preflight must pass with a file-ref-only credentials: entry and no ambient env var: %v", err)
	}
	if info.Version == "" {
		t.Fatal("preflight returned no version")
	}
	found := false
	for _, kv := range runner.authProbeEnv {
		if kv == "COPILOT_GITHUB_TOKEN=pat-from-file-ref-only" {
			found = true
		}
	}
	if !found {
		t.Fatalf("sign-in probe env should carry the file-ref credential; got %v", runner.authProbeEnv)
	}
}
