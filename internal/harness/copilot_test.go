package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	copilotsdk "github.com/github/copilot-sdk/go"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/procenv"
)

const (
	milestoneHelperEnv    = "GOOBERS_TEST_MILESTONE_HELPER"
	milestoneHelperMarker = "GOOBERS_TEST_MILESTONE_MARKER"
)

func TestMain(m *testing.M) {
	if os.Getenv(milestoneHelperEnv) == "1" {
		want := []string{"set-milestone", "--item", "7", "--milestone", "22"}
		if !slices.Equal(os.Args[1:], want) {
			fmt.Fprintf(os.Stderr, "milestone helper args = %q, want %q\n", os.Args[1:], want)
			os.Exit(2)
		}
		marker := os.Getenv(milestoneHelperMarker)
		if marker == "" {
			fmt.Fprintln(os.Stderr, "milestone helper marker is empty")
			os.Exit(2)
		}
		if err := os.WriteFile(marker, []byte("invoked"), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "write milestone helper marker: %v\n", err)
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeProcessRunner is a scripted ProcessRunner double: it lets tests inspect
// the built command/env/dir and script an arbitrary side effect (e.g. writing
// the completion file, as a real CLI would) without a real subprocess.
type fakeProcessRunner struct {
	lastReq ProcessRequest
	act     func(req ProcessRequest) error
	result  ProcessResult
	err     error
}

type fakeCopilotModelLister struct {
	models    []CopilotModelInfo
	responses [][]CopilotModelInfo
	err       error
	calls     int
}

func (f *fakeCopilotModelLister) ListModels(context.Context, []string, []string) ([]CopilotModelInfo, error) {
	response := f.models
	if len(f.responses) > 0 {
		response = f.responses[min(f.calls, len(f.responses)-1)]
	}
	f.calls++
	return append([]CopilotModelInfo(nil), response...), f.err
}

func testCopilotModelList() []CopilotModelInfo {
	maxEffort := []string{"none", "low", "medium", "high", "xhigh", "max"}
	return []CopilotModelInfo{
		{ID: "claude-fable-5", SupportedReasoningEfforts: maxEffort},
		{ID: "claude-sonnet-5", SupportedReasoningEfforts: maxEffort},
		{ID: "claude-sonnet-4.6", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "max"}},
		{ID: "claude-sonnet-4.5"},
		{ID: "claude-opus-4.8-fast", SupportedReasoningEfforts: maxEffort},
		{ID: "claude-opus-4.5"},
		{ID: "future-model"},
		{ID: "kimi-k2.7-code"},
		{ID: "mai-code-1-flash-picker"},
	}
}

func testHarnessOptions(t *testing.T, values map[string]interface{}) map[string]apiextensionsv1.JSON {
	t.Helper()
	options := make(map[string]apiextensionsv1.JSON, len(values))
	for name, value := range values {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal harness option %q: %v", name, err)
		}
		options[name] = apiextensionsv1.JSON{Raw: raw}
	}
	return options
}

func (f *fakeProcessRunner) Run(ctx context.Context, req ProcessRequest) (ProcessResult, error) {
	f.lastReq = req
	if f.act != nil {
		if err := f.act(req); err != nil {
			return f.result, err
		}
	}
	return f.result, f.err
}

// pushCredentials builds a *credentials.Set materialized for "repo:push",
// backed by a real env-var token ref, for tests exercising credential
// injection into the CLI subprocess.
func pushCredentials(t *testing.T, capability, token string) *credentials.Set {
	t.Helper()
	t.Setenv("PUSH_TOKEN_ENV", token)
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "push-ref", Env: "PUSH_TOKEN_ENV"}})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	injector, err := credentials.NewInjector(resolver, []credentials.Grant{{Capability: capability, Ref: "push-ref"}}, noopRegistrar{})
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	set, err := injector.Materialize(context.Background(), []string{capability})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	return set
}

// twoTokenCredentials materializes a *credentials.Set granting two distinct
// capabilities from two distinct token refs — the multi-token case #288 wires,
// where a stage holds a personal Copilot-Requests PAT for the model alongside
// an org-repo token for the github tool.
func twoTokenCredentials(t *testing.T, capA, tokA, capB, tokB string) *credentials.Set {
	t.Helper()
	t.Setenv("TOK_A_ENV", tokA)
	t.Setenv("TOK_B_ENV", tokB)
	resolver, err := credentials.NewResolver([]credentials.TokenRef{
		{Name: "ref-a", Env: "TOK_A_ENV"},
		{Name: "ref-b", Env: "TOK_B_ENV"},
	})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	injector, err := credentials.NewInjector(resolver, []credentials.Grant{
		{Capability: capA, Ref: "ref-a"},
		{Capability: capB, Ref: "ref-b"},
	}, noopRegistrar{})
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	set, err := injector.Materialize(context.Background(), []string{capA, capB})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	return set
}

// TestCopilotAdapterInjectsModelAndGitHubTokensTogether is #288's core property
// (§3.3): a stage declaring both agent:model and an org-repo capability carries
// BOTH tokens into one subprocess under DISTINCT env vars — the model token as
// COPILOT_GITHUB_TOKEN (which the Copilot CLI prefers for model auth) and the
// github-tool token as GH_TOKEN — so neither clobbers the other. This is the
// two-tokens-one-subprocess case the agentic curate stage needs at #30.
func TestCopilotAdapterInjectsModelAndGitHubTokensTogether(t *testing.T) {
	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CopilotAdapter{
		Command: []string{"copilot"},
		Runner:  runner,
		EnvCapabilities: map[string]string{
			"agent:model":         "COPILOT_GITHUB_TOKEN",
			"github:issues:write": "GH_TOKEN",
		},
	}
	creds := twoTokenCredentials(t, "agent:model", "copilot-pat", "github:issues:write", "org-repo-token")
	req := RunRequest{
		Envelope:       testEnvelope(workspace, "agent:model", "github:issues:write"),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    creds,
	}
	if _, err := adapter.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	gotModel, gotGitHub := false, false
	for _, kv := range runner.lastReq.Env {
		switch kv {
		case "COPILOT_GITHUB_TOKEN=copilot-pat":
			gotModel = true
		case "GH_TOKEN=org-repo-token":
			gotGitHub = true
		}
	}
	if !gotModel || !gotGitHub {
		t.Fatalf("expected both COPILOT_GITHUB_TOKEN=copilot-pat and GH_TOKEN=org-repo-token in one subprocess env, got %v", runner.lastReq.Env)
	}
	for _, arg := range runner.lastReq.Command {
		if strings.Contains(arg, "copilot-pat") || strings.Contains(arg, "org-repo-token") {
			t.Fatalf("token leaked into argv: %v", runner.lastReq.Command)
		}
	}
}

func TestCopilotAdapterInjectsMilestoneCommandEnvironment(t *testing.T) {
	workspace := t.TempDir()
	selfBin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(workspace, "milestone-invoked")
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			var goobersBin string
			for _, entry := range req.Env {
				if value, ok := strings.CutPrefix(entry, executor.GoobersBinEnvVar+"="); ok {
					goobersBin = value
					break
				}
			}
			if goobersBin == "" {
				return errors.New("GOOBERS_BIN is missing")
			}
			cmd := exec.Command(goobersBin, "set-milestone", "--item", "7", "--milestone", "22")
			cmd.Env = append(req.Env,
				milestoneHelperEnv+"=1",
				milestoneHelperMarker+"="+marker,
			)
			if output, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("invoke resolved milestone command: %w: %s", err, output)
			}
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CopilotAdapter{
		Command:      []string{"copilot"},
		Runner:       runner,
		InstanceRoot: "/instances/acme",
		SelfBin:      selfBin,
		EnvCapabilities: map[string]string{
			"github:issues:write":     "GH_TOKEN",
			"github:milestones:write": executor.CredentialEnvVar("github:milestones:write"),
		},
	}
	creds := twoTokenCredentials(t,
		"github:issues:write", "issues-token",
		"github:milestones:write", "milestones-token",
	)
	req := RunRequest{
		Envelope:       testEnvelope(workspace, "github:issues:write", "github:milestones:write"),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    creds,
	}
	if _, err := adapter.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []string{
		"GH_TOKEN=issues-token",
		"GOOBERS_CRED_GITHUB_MILESTONES_WRITE=milestones-token",
		executor.InstanceRootEnvVar + "=/instances/acme",
		executor.GoobersBinEnvVar + "=" + selfBin,
		executor.RepoProviderEnvVar + "=github",
		executor.RepoOwnerEnvVar + "=acme",
		executor.RepoNameEnvVar + "=web",
	}
	for _, entry := range want {
		if !slices.Contains(runner.lastReq.Env, entry) {
			t.Errorf("subprocess env missing %q: %v", entry, runner.lastReq.Env)
		}
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "invoked" {
		t.Fatalf("resolved milestone command marker = %q, %v", got, err)
	}
}

func TestCopilotAdapterDoesNotUseAnotherGoobersGrantWhenStoredAuthIsAllowed(t *testing.T) {
	t.Setenv("OTHER_GOOBER_TOKEN", "other-goober-token")
	resolver, err := credentials.NewResolver([]credentials.TokenRef{
		{Name: "other-goober", Env: "OTHER_GOOBER_TOKEN"},
	})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	injector, err := credentials.NewGooberInjector(resolver, "goober-a", []credentials.Grant{
		{Goober: "goober-b", Capability: "agent:model", Ref: "other-goober"},
	}, noopRegistrar{})
	if err != nil {
		t.Fatalf("NewGooberInjector: %v", err)
	}
	creds, err := injector.Materialize(context.Background(), []string{"agent:model"})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CopilotAdapter{
		Command:                        []string{"copilot"},
		Runner:                         runner,
		EnvCapabilities:                map[string]string{"agent:model": "COPILOT_GITHUB_TOKEN"},
		OptionalCredentialCapabilities: map[string]bool{"agent:model": true},
	}
	_, err = adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace, "agent:model"),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    creds,
	})
	if err != nil {
		t.Fatalf("Run with stored auth fallback: %v", err)
	}
	for _, entry := range runner.lastReq.Env {
		if entry == "COPILOT_GITHUB_TOKEN=other-goober-token" {
			t.Fatalf("another goober's grant leaked into subprocess env: %v", runner.lastReq.Env)
		}
	}
}

func TestCopilotAdapterUsesStoredAuthWhenAgentModelGrantIsAbsent(t *testing.T) {
	t.Setenv("USERPROFILE", `C:\Users\operator`)
	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatal(err)
	}

	injector, err := credentials.NewGooberInjector(resolver, "goober-a", nil, noopRegistrar{})
	if err != nil {
		t.Fatal(err)
	}
	creds, err := injector.Materialize(context.Background(), []string{"agent:model"})
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CopilotAdapter{
		Command:                        []string{"copilot"},
		Runner:                         runner,
		EnvCapabilities:                map[string]string{"agent:model": "COPILOT_GITHUB_TOKEN"},
		OptionalCredentialCapabilities: map[string]bool{"agent:model": true},
	}
	if _, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace, "agent:model"),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    creds,
	}); err != nil {
		t.Fatalf("Run with stored auth: %v", err)
	}
	hasProfile := false
	for _, entry := range runner.lastReq.Env {
		if entry == `USERPROFILE=C:\Users\operator` {
			hasProfile = true
		}
	}
	if !hasProfile {
		t.Fatalf("stored-auth profile location missing from env: %v", runner.lastReq.Env)
	}
	for _, entry := range runner.lastReq.Env {
		if strings.HasPrefix(entry, "COPILOT_GITHUB_TOKEN=") {
			t.Fatalf("unexpected model token injected during stored auth: %v", runner.lastReq.Env)
		}
	}
}

func TestCopilotAdapterRejectsStoredAuthWithRepositoryToken(t *testing.T) {
	workspace := t.TempDir()
	runner := &fakeProcessRunner{}
	adapter := &CopilotAdapter{
		Command: []string{"copilot"},
		Runner:  runner,
		EnvCapabilities: map[string]string{
			"agent:model": "COPILOT_GITHUB_TOKEN",
			"repo:push":   "GH_TOKEN",
		},
		OptionalCredentialCapabilities: map[string]bool{"agent:model": true},
	}
	t.Setenv("PUSH_TOKEN_ENV", "repository-token")
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "push-ref", Env: "PUSH_TOKEN_ENV"}})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	injector, err := credentials.NewInjector(resolver, []credentials.Grant{
		{Capability: "repo:push", Ref: "push-ref"},
	}, noopRegistrar{})
	if err != nil {
		t.Fatalf("NewGooberInjector: %v", err)
	}
	creds, err := injector.Materialize(context.Background(), []string{"agent:model", "repo:push"})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	_, err = adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace, "agent:model", "repo:push"),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    creds,
	})
	if err == nil || !strings.Contains(err.Error(), "configure a distinct agent:model credential") {
		t.Fatalf("Run error = %v, want actionable stored-auth conflict", err)
	}
	if len(runner.lastReq.Command) != 0 {
		t.Fatalf("Copilot launched despite stored-auth conflict: %+v", runner.lastReq)
	}
}

func TestCopilotAdapterStillFailsClosedForMissingRequiredCredential(t *testing.T) {
	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	injector, err := credentials.NewGooberInjector(resolver, "goober-a", nil, noopRegistrar{})
	if err != nil {
		t.Fatal(err)
	}
	creds, err := injector.Materialize(context.Background(), []string{"github:issues:write"})
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeProcessRunner{}
	adapter := &CopilotAdapter{
		Command:         []string{"copilot"},
		Runner:          runner,
		EnvCapabilities: map[string]string{"github:issues:write": "GH_TOKEN"},
	}
	_, err = adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(t.TempDir(), "github:issues:write"),
		Workspace:      t.TempDir(),
		CompletionPath: DefaultResultPath,
		Credentials:    creds,
	})
	if !errors.Is(err, credentials.ErrNoCredentialForCapability) {
		t.Fatalf("Run error = %v, want ErrNoCredentialForCapability", err)
	}
	if len(runner.lastReq.Command) != 0 {
		t.Fatalf("subprocess ran without required credential: %+v", runner.lastReq)
	}
}

func TestCredentialEnvToleratesMissingRepoPushOnADO(t *testing.T) {
	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	injector, err := credentials.NewGooberInjector(resolver, "goober-a", nil, noopRegistrar{})
	if err != nil {
		t.Fatal(err)
	}
	creds, err := injector.Materialize(context.Background(), []string{"repo:push"})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &CopilotAdapter{
		Command:         []string{"copilot"},
		EnvCapabilities: map[string]string{"repo:push": "GOOBERS_REPO_TOKEN"},
	}
	env := testEnvelope(t.TempDir(), "repo:push")
	env.RepoRef = apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "example-org", Project: "Example Service", Name: "Example.Repo"}
	got, err := adapter.credentialEnv(context.Background(), RunRequest{
		Envelope:    env,
		Workspace:   t.TempDir(),
		Credentials: creds,
	})
	if err != nil {
		t.Fatalf("credentialEnv on ADO repo = %v, want nil (repo:push tolerated)", err)
	}
	for _, kv := range got {
		if strings.HasPrefix(kv, "GOOBERS_REPO_TOKEN=") {
			t.Fatalf("repo:push token injected without a grant: %q", kv)
		}
	}
	if !containsEnv(got, executor.RepoProjectEnvVar+"=Example Service") {
		t.Fatalf("ADO project not injected into harness env: %v", got)
	}
}

func TestCredentialEnvFailsClosedForMissingRepoPushOnGitHub(t *testing.T) {
	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	injector, err := credentials.NewGooberInjector(resolver, "goober-a", nil, noopRegistrar{})
	if err != nil {
		t.Fatal(err)
	}
	creds, err := injector.Materialize(context.Background(), []string{"repo:push"})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &CopilotAdapter{
		Command:         []string{"copilot"},
		EnvCapabilities: map[string]string{"repo:push": "GOOBERS_REPO_TOKEN"},
	}
	env := testEnvelope(t.TempDir(), "repo:push")
	_, err = adapter.credentialEnv(context.Background(), RunRequest{
		Envelope:    env,
		Workspace:   t.TempDir(),
		Credentials: creds,
	})
	if !errors.Is(err, credentials.ErrNoCredentialForCapability) {
		t.Fatalf("credentialEnv on GitHub repo = %v, want ErrNoCredentialForCapability", err)
	}
}

func containsEnv(env []string, want string) bool {
	for _, kv := range env {
		if kv == want {
			return true
		}
	}
	return false
}

func TestCopilotAdapterRendersPromptAndCollectsResult(t *testing.T) {
	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{Transcript: []byte("copilot: implementing...\ncopilot: done."), ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "ok"})
		},
	}
	adapter := &CopilotAdapter{
		Command:         []string{"copilot"},
		Runner:          runner,
		EnvCapabilities: map[string]string{"repo:push": "GH_TOKEN"},
	}

	creds := pushCredentials(t, "repo:push", "push-token-value")
	env := testEnvelope(workspace, "repo:push")
	req := RunRequest{
		Mode:           ModeInvoke,
		Envelope:       env,
		Instructions:   "You are a coder.",
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    creds,
		Timeout:        5 * time.Second,
	}

	out, err := adapter.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(out.Payload) == 0 {
		t.Fatal("expected a non-empty result payload")
	}
	if string(out.Transcript) != "copilot: implementing...\ncopilot: done." {
		t.Fatalf("transcript = %q", out.Transcript)
	}

	// The command contains PromptFlag + prompt text + extras. On Windows the
	// base command is the PowerShell npm shim rather than bare "copilot".
	promptIndex := slices.Index(runner.lastReq.Command, defaultPromptFlag)
	if promptIndex < 0 || promptIndex+1 >= len(runner.lastReq.Command) {
		t.Fatalf("unexpected command: %v", runner.lastReq.Command)
	}
	promptText := runner.lastReq.Command[promptIndex+1]
	if !strings.Contains(promptText, "You are a coder.") {
		t.Fatalf("prompt missing instructions: %q", promptText)
	}
	if !strings.Contains(promptText, req.Envelope.Goal) {
		t.Fatalf("prompt missing goal: %q", promptText)
	}
	if !strings.Contains(promptText, DefaultResultPath) {
		t.Fatalf("prompt missing completion path directive: %q", promptText)
	}
	found := false
	for _, arg := range runner.lastReq.Command {
		if arg == "--allow-all-tools" {
			found = true
		}
		if strings.HasPrefix(arg, "--available-tools") {
			t.Fatalf("empty tool declaration changed the default command: %v", runner.lastReq.Command)
		}
	}
	if !found {
		t.Fatalf("expected --allow-all-tools in default extra args: %v", runner.lastReq.Command)
	}
	// The prompt is also written to the workspace for human debugging.
	debugPrompt, err := os.ReadFile(filepath.Join(workspace, ".goobers", "prompt.md"))
	if err != nil {
		t.Fatalf("read debug prompt: %v", err)
	}
	if string(debugPrompt) != promptText {
		t.Fatalf("debug prompt file does not match the prompt sent to the CLI")
	}

	// The credential was injected as an env var, not a CLI arg.
	foundEnv := false
	for _, kv := range runner.lastReq.Env {
		if kv == "GH_TOKEN=push-token-value" {
			foundEnv = true
		}
	}
	if !foundEnv {
		t.Fatalf("expected GH_TOKEN=push-token-value in subprocess env, got %v", runner.lastReq.Env)
	}
	telemetryPrefix := "GOOBERS_TELEMETRY_DIR="
	foundTelemetryDir := false
	for _, kv := range runner.lastReq.Env {
		if strings.HasPrefix(kv, telemetryPrefix) {
			foundTelemetryDir = true
			if info, err := os.Stat(strings.TrimPrefix(kv, telemetryPrefix)); err != nil || !info.IsDir() {
				t.Fatalf("telemetry dir is not writable stage storage: %q (%v)", kv, err)
			}
		}
	}
	if !foundTelemetryDir {
		t.Fatalf("expected GOOBERS_TELEMETRY_DIR in subprocess env, got %v", runner.lastReq.Env)
	}
	for _, arg := range runner.lastReq.Command {
		if strings.Contains(arg, "push-token-value") {
			t.Fatalf("token leaked into argv: %v", runner.lastReq.Command)
		}
	}
}

func TestCopilotAdapterToolAllowlist(t *testing.T) {
	tests := []struct {
		name             string
		model            string
		harnessOptions   map[string]apiextensionsv1.JSON
		tools            []string
		wantAvailable    []string
		wantIncluded     []string
		wantOmitted      []string
		wantCommandParts []string
		wantIssues       bool
		externalMCP      bool
	}{
		{
			name:  "explicit empty preserves default",
			tools: []string{},
		},
		{
			name:          "concrete tools omit undeclared mutation",
			tools:         []string{"view", "glob"},
			wantAvailable: []string{"view", "glob"},
			wantOmitted:   []string{"create"},
		},
		{
			name:           "model configuration survives tool constraints",
			model:          "claude-sonnet-5",
			harnessOptions: testHarnessOptions(t, map[string]interface{}{"context": "long_context", "reasoningEffort": "xhigh"}),
			tools:          []string{"view"},
			wantAvailable:  []string{"view"},
			wantCommandParts: []string{
				"--model claude-sonnet-5",
				"--context long_context",
				"--reasoning-effort xhigh",
			},
		},
		{
			name:         "shipped shell group expands",
			tools:        []string{"shell"},
			wantIncluded: []string{"bash", "view", "create", "apply_patch", "rg", "glob"},
			wantOmitted:  []string{"github-mcp-server-issue_write"},
		},
		{
			name:         "shipped github group excludes shell",
			tools:        []string{"github"},
			wantIncluded: []string{"github-mcp-server-issue_read", "github-mcp-server-issue_write"},
			wantOmitted:  []string{"bash", "powershell"},
			wantIssues:   true,
		},
		{
			name:         "shipped github and telemetry groups expand independently",
			tools:        []string{"github", "telemetry"},
			wantIncluded: []string{"github-mcp-server-issue_read", "github-mcp-server-issue_write", "github-mcp-server-add_issue_comment", "github-mcp-server-search_issues", "view", "rg"},
			wantOmitted:  []string{"bash", "powershell", "create"},
			wantIssues:   true,
		},
		{
			name:         "external MCP tools are server-qualified without disabling declared GitHub",
			tools:        []string{"github", "reachability"},
			wantIncluded: []string{"github-mcp-server-issue_read", "context-github", "context-reachability"},
			wantOmitted:  []string{"create", "context-github-mcp-server-issue_read"},
			wantIssues:   true,
			externalMCP:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			var capturedStdout bool
			runner := &fakeProcessRunner{
				result: ProcessResult{ExitCode: 0},
				act: func(req ProcessRequest) error {
					if len(tc.tools) == 0 {
						if req.StdoutCapture != nil {
							t.Fatal("empty tool declaration configured response capture")
						}
						return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
					}
					if req.StdoutCapture == nil {
						return errors.New("tool-constrained run did not capture stdout")
					}
					capturedStdout = true
					_, err := req.StdoutCapture.Write([]byte(`{"status":"success","summary":"done"}`))
					return err
				},
			}
			adapter := &CopilotAdapter{
				Command:         []string{"copilot"},
				Runner:          runner,
				EnvCapabilities: map[string]string{"agent:model": "COPILOT_GITHUB_TOKEN"},
			}
			envelope := testEnvelope(workspace)
			var mcpServers []apiv1.MCPServer
			var creds *credentials.Set
			if tc.externalMCP {
				envelope = testEnvelope(workspace, "agent:model")
				mcpServers = []apiv1.MCPServer{{Name: "context", Command: "context-server"}}
				creds = mcpTestCredentials(t, "agent:model", "model-token")
			}

			out, err := adapter.Run(context.Background(), RunRequest{
				Envelope:              envelope,
				Model:                 tc.model,
				HarnessOptions:        tc.harnessOptions,
				HarnessConfigResolved: true,
				Workspace:             workspace,
				CompletionPath:        DefaultResultPath,
				Tools:                 tc.tools,
				MCPServers:            mcpServers,
				Credentials:           creds,
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			const prefix = "--available-tools="
			var available []string
			for _, arg := range runner.lastReq.Command {
				if value, ok := strings.CutPrefix(arg, prefix); ok {
					available = strings.Split(value, ",")
				}
			}
			if len(tc.tools) == 0 {
				if available != nil || slices.Contains(runner.lastReq.Command, "--silent") {
					t.Fatalf("empty tool declaration changed the default command: %v", runner.lastReq.Command)
				}
				return
			}
			if !capturedStdout {
				t.Fatal("tool-constrained run did not use response completion")
			}
			if tc.wantAvailable != nil && !slices.Equal(available, tc.wantAvailable) {
				t.Fatalf("available tools = %v, want %v", available, tc.wantAvailable)
			}
			command := strings.Join(runner.lastReq.Command, " ")
			for _, want := range tc.wantCommandParts {
				if !strings.Contains(command, want) {
					t.Errorf("command = %q, want %q", command, want)
				}
			}
			for _, want := range tc.wantIncluded {
				if !slices.Contains(available, want) {
					t.Errorf("available tools missing %q: %v", want, available)
				}
			}
			for _, omitted := range tc.wantOmitted {
				if slices.Contains(available, omitted) {
					t.Fatalf("omitted tool %q is available: %v", omitted, available)
				}
			}
			if !slices.Contains(runner.lastReq.Command, "--allow-all-tools") {
				t.Fatalf("non-interactive permission flag missing: %v", runner.lastReq.Command)
			}
			if !slices.Contains(runner.lastReq.Command, "--silent") ||
				!slices.Contains(runner.lastReq.Command, "--output-format=text") {
				t.Fatalf("response completion flags missing: %v", runner.lastReq.Command)
			}
			if got := slices.Contains(runner.lastReq.Command, "--add-github-mcp-toolset=issues"); got != tc.wantIssues {
				t.Fatalf("GitHub issues toolset enabled = %t, want %t: %v", got, tc.wantIssues, runner.lastReq.Command)
			}
			if tc.externalMCP && slices.Contains(runner.lastReq.Command, "--disable-builtin-mcps") {
				t.Fatalf("declared GitHub group was disabled by external MCP isolation: %v", runner.lastReq.Command)
			}
			promptIndex := slices.Index(runner.lastReq.Command, defaultPromptFlag)
			if promptIndex < 0 || promptIndex+1 >= len(runner.lastReq.Command) {
				t.Fatalf("command missing prompt: %v", runner.lastReq.Command)
			}
			prompt := runner.lastReq.Command[promptIndex+1]
			if !strings.Contains(prompt, "return your result as the entire final response") ||
				strings.Contains(prompt, "write your result as JSON to") {
				t.Fatalf("tool-constrained prompt does not use response completion: %q", prompt)
			}
			if got := string(out.Payload); got != `{"status":"success","summary":"done"}` {
				t.Fatalf("payload = %q", got)
			}
			if _, err := os.Stat(filepath.Join(workspace, DefaultResultPath)); !os.IsNotExist(err) {
				t.Fatalf("tool-constrained run wrote a completion file: %v", err)
			}
		})
	}
}

func TestCopilotAdapterConstrainedRunUsesAdmittedFallback(t *testing.T) {
	modelLister := &fakeCopilotModelLister{models: []CopilotModelInfo{{ID: "available-model"}}}
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			if req.StdoutCapture == nil {
				return errors.New("tool-constrained run did not capture stdout")
			}
			_, err := req.StdoutCapture.Write([]byte(`{"status":"success","summary":"done"}`))
			return err
		},
	}
	adapter := &CopilotAdapter{
		Command:     []string{"copilot"},
		ModelLister: modelLister,
		Runner:      runner,
	}
	resolution, err := adapter.ResolveConfig("retired-model", testHarnessOptions(t, map[string]interface{}{
		fallbackToDefaultOption: true,
	}))
	if err != nil {
		t.Fatalf("ResolveConfig: %v", err)
	}

	workspace := t.TempDir()
	if _, err := adapter.Run(context.Background(), RunRequest{
		Envelope:              testEnvelope(workspace),
		Model:                 resolution.Model,
		HarnessOptions:        resolution.HarnessOptions,
		HarnessConfigResolved: true,
		Workspace:             workspace,
		CompletionPath:        DefaultResultPath,
		Tools:                 []string{"view"},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if modelLister.calls != 1 {
		t.Fatalf("model discovery calls = %d, want admission query only", modelLister.calls)
	}
	if slices.Contains(runner.lastReq.Command, "--model") {
		t.Fatalf("command = %q, want admitted harness default", runner.lastReq.Command)
	}
	if !slices.Contains(runner.lastReq.Command, "--available-tools=view") ||
		!slices.Contains(runner.lastReq.Command, "--output-format=text") {
		t.Fatalf("command = %q, want constrained response completion", runner.lastReq.Command)
	}
}

func TestCopilotAdapterConstrainedTranscriptUsesSentPrompt(t *testing.T) {
	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{
			ExitCode:   0,
			Transcript: []byte("copilot completed the task"),
		},
		act: func(req ProcessRequest) error {
			if req.StdoutCapture == nil {
				return errors.New("tool-constrained run did not capture stdout")
			}
			_, err := req.StdoutCapture.Write([]byte(`{"status":"success","summary":"done"}`))
			return err
		},
	}
	adapter := &CopilotAdapter{
		Command: []string{"copilot"},
		Runner:  runner,
	}
	rec := &fakeRecorder{}
	exec, err := NewExecutor(
		adapter,
		testInjector(t, "", "", noopRegistrar{}),
		rec,
		rec,
		rec,
		journal.NewPatternScrubber(),
		"",
		WithTools([]string{"view"}),
	)
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}

	if _, err := exec.Invoke(context.Background(), testEnvelope(workspace)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(rec.spans) != 1 {
		t.Fatalf("recorded spans = %d, want 1", len(rec.spans))
	}
	events := decodeTranscriptEvents(t, rec.spans[0].data)
	if len(events) != 2 {
		t.Fatalf("transcript events = %#v, want user and assistant events", events)
	}
	promptIndex := slices.Index(runner.lastReq.Command, defaultPromptFlag)
	if promptIndex < 0 || promptIndex+1 >= len(runner.lastReq.Command) {
		t.Fatalf("command missing prompt: %v", runner.lastReq.Command)
	}
	if events[0].Role != "user" || events[0].Content != runner.lastReq.Command[promptIndex+1] {
		t.Fatalf("transcript prompt = %#v, want exact command prompt", events[0])
	}
	if !strings.Contains(events[0].Content, "return your result as the entire final response") ||
		strings.Contains(events[0].Content, "write your result as JSON to") {
		t.Fatalf("transcript recorded the wrong completion contract: %q", events[0].Content)
	}
}

func TestCopilotToolAllowlistPreservesShippedCuratorContract(t *testing.T) {
	for _, path := range []string{
		filepath.Join("..", "..", "config-examples", "gaggles", "acme-web", "goobers", "curator", "goober.yaml"),
		filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers", "goobers", "curator", "goober.yaml"),
	} {
		t.Run(path, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read curator definition: %v", err)
			}
			var curator apiv1.Goober
			if err := yaml.Unmarshal(raw, &curator); err != nil {
				t.Fatalf("unmarshal curator definition: %v", err)
			}
			if want := []string{"github", "shell"}; !slices.Equal(curator.Spec.Tools, want) {
				t.Fatalf("curator tools = %v, want %v", curator.Spec.Tools, want)
			}

			available := copilotAvailableTools(RunRequest{Tools: curator.Spec.Tools})
			for _, required := range []string{
				"github-mcp-server-add_issue_comment",
				"github-mcp-server-issue_read",
				"github-mcp-server-issue_write",
				"github-mcp-server-sub_issue_write",
				"view",
				"bash",
			} {
				if !slices.Contains(available, required) {
					t.Errorf("curator tool expansion missing %q: %v", required, available)
				}
			}
		})
	}
}

func TestCopilotToolAllowlistPreservesShippedNominatorApprovalContract(t *testing.T) {
	for _, path := range []string{
		filepath.Join("..", "..", "config-examples", "gaggles", "acme-web", "goobers", "nominator", "goober.yaml"),
		filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers", "goobers", "nominator", "goober.yaml"),
	} {
		t.Run(path, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read nominator definition: %v", err)
			}
			var nominator apiv1.Goober
			if err := yaml.Unmarshal(raw, &nominator); err != nil {
				t.Fatalf("unmarshal nominator definition: %v", err)
			}
			if want := []string{"github", "telemetry", "shell"}; !slices.Equal(nominator.Spec.Tools, want) {
				t.Fatalf("nominator tools = %v, want %v", nominator.Spec.Tools, want)
			}

			available := copilotAvailableTools(RunRequest{Tools: nominator.Spec.Tools})
			for _, required := range []string{
				"github-mcp-server-add_issue_comment",
				"github-mcp-server-issue_write",
				"github-mcp-server-sub_issue_write",
				"view",
				"bash",
			} {
				if !slices.Contains(available, required) {
					t.Errorf("nominator tool expansion missing %q: %v", required, available)
				}
			}
		})
	}
}

func TestCopilotAdapterEmptyToolAllowlistPreservesCommand(t *testing.T) {
	run := func(tools []string) []string {
		workspace := t.TempDir()
		runner := &fakeProcessRunner{
			result: ProcessResult{ExitCode: 0},
			act: func(req ProcessRequest) error {
				return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
			},
		}
		adapter := &CopilotAdapter{
			Command: []string{"copilot", "--session-id", "00000000-0000-4000-8000-000000000001"},
			Runner:  runner,
		}
		if _, err := adapter.Run(context.Background(), RunRequest{
			Envelope:       testEnvelope(workspace),
			Workspace:      workspace,
			CompletionPath: DefaultResultPath,
			Tools:          tools,
		}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		return runner.lastReq.Command
	}

	absent := run(nil)
	empty := run([]string{})
	if !slices.Equal(absent, empty) {
		t.Fatalf("empty tool allowlist changed command:\nabsent: %v\nempty:  %v", absent, empty)
	}
}

func TestCopilotAdapterToolAllowlistRejectsCommaDelimitedEntry(t *testing.T) {
	runner := &fakeProcessRunner{}
	workspace := t.TempDir()
	adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}

	_, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Tools:          []string{"view,bash"},
	})
	if err == nil || !strings.Contains(err.Error(), "must not contain a comma") {
		t.Fatalf("Run error = %v, want comma-delimited tool rejection", err)
	}
	if len(runner.lastReq.Command) != 0 {
		t.Fatalf("process ran with ambiguous tool allowlist: %v", runner.lastReq.Command)
	}
}

func TestCopilotAdapterToolAllowlistRejectsConflictingConfiguredArgs(t *testing.T) {
	for _, conflictingArg := range []string{"--available-tools=view", "--output-format=json"} {
		t.Run(conflictingArg, func(t *testing.T) {
			runner := &fakeProcessRunner{}
			workspace := t.TempDir()
			adapter := &CopilotAdapter{
				Command:   []string{"copilot"},
				ExtraArgs: []string{conflictingArg},
				Runner:    runner,
			}

			_, err := adapter.Run(context.Background(), RunRequest{
				Envelope:       testEnvelope(workspace),
				Workspace:      workspace,
				CompletionPath: DefaultResultPath,
				Tools:          []string{"view"},
			})
			if err == nil || !strings.Contains(err.Error(), conflictingArg) {
				t.Fatalf("Run error = %v, want configured argument conflict", err)
			}
			if len(runner.lastReq.Command) != 0 {
				t.Fatalf("process ran with conflicting tool constraints: %v", runner.lastReq.Command)
			}
		})
	}
}

func TestCopilotAdapterRecoversInvalidResponseCompletionInSameSession(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("COPILOT_HOME", "")
	workspace := t.TempDir()
	var calls []ProcessRequest
	runner := &fakeProcessRunner{result: ProcessResult{ExitCode: 0}}
	runner.act = func(req ProcessRequest) error {
		calls = append(calls, req)
		if len(calls) == 1 {
			_, _ = req.StdoutCapture.Write([]byte(`{"message":"not a result envelope"}`))
			if err := os.MkdirAll(filepath.Dir(filepath.Join(req.Dir, DefaultResultPath)), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(req.Dir, DefaultResultPath), []byte(`{}`), 0o600); err != nil {
				return err
			}
		} else {
			_, _ = req.StdoutCapture.Write([]byte(`{"status":"success","outputs":{},"summary":"done","metrics":{}}`))
		}
		return nil
	}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}

	out, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Tools:          []string{"view"},
		Timeout:        time.Minute,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := string(out.Payload); got != `{"status":"success","outputs":{},"summary":"done","metrics":{}}` {
		t.Fatalf("payload = %q", got)
	}
	if len(calls) != 2 {
		t.Fatalf("process calls = %d, want initial call plus one recovery", len(calls))
	}
	firstSession, firstOK := nativeSessionID(calls[0])
	secondSession, secondOK := nativeSessionID(calls[1])
	if !firstOK || !secondOK || firstSession != secondSession {
		t.Fatalf("recovery did not resume the initial session: first=%q second=%q", firstSession, secondSession)
	}
	promptIndex := slices.Index(calls[1].Command, defaultPromptFlag)
	if promptIndex < 0 || promptIndex+1 >= len(calls[1].Command) {
		t.Fatalf("recovery command missing prompt: %v", calls[1].Command)
	}
	if prompt := calls[1].Command[promptIndex+1]; !strings.Contains(prompt, "entire response") ||
		!strings.Contains(prompt, "without returning the mandatory completion") {
		t.Fatalf("recovery prompt = %q", prompt)
	}
}

func TestCopilotAdapterResponseCompletionRejectsTruncation(t *testing.T) {
	capture := newTranscriptBuffer(8)
	_, _ = capture.Write([]byte(`{"status":"success"}`))

	_, err := readCopilotResponseCompletion(ModeInvoke, capture)
	if !errors.Is(err, ErrNoCompletion) {
		t.Fatalf("read completion error = %v, want ErrNoCompletion", err)
	}
}

func TestCopilotAdapterRecoversMissingCompletionInSameSession(t *testing.T) {
	for _, tc := range []struct {
		name           string
		mode           Mode
		completionPath string
		completion     interface{}
	}{
		{
			name:           "result",
			mode:           ModeInvoke,
			completionPath: DefaultResultPath,
			completion:     apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "done"},
		},
		{
			name:           "verdict",
			mode:           ModeReview,
			completionPath: DefaultVerdictPath,
			completion:     apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "approved"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", "")
			t.Setenv("COPILOT_HOME", "")
			workspace := t.TempDir()
			var calls []ProcessRequest
			runner := &fakeProcessRunner{
				result: ProcessResult{Transcript: []byte("finished"), ExitCode: 0},
				act: func(req ProcessRequest) error {
					calls = append(calls, req)
					if len(calls) == 1 {
						// The adapter derives the recovery turn's timeout from
						// time.Since(started) measured around this call (see
						// CopilotAdapter.Run's "remaining := totalTimeout -
						// time.Since(started)"). A fake runner that returns
						// instantly can complete within a single tick of a
						// coarse OS clock, making the elapsed duration read
						// back as exactly zero — which would hand the
						// recovery turn the *entire* original timeout rather
						// than a genuine remainder, and is exactly what was
						// observed on Windows CI. Sleep briefly so the first
						// turn measurably consumes wall-clock time on every
						// platform, the same way a real subprocess turn
						// always would.
						time.Sleep(5 * time.Millisecond)
					}
					if len(calls) == 2 {
						return WriteCompletion(req.Dir, tc.completionPath, tc.completion)
					}
					return nil
				},
			}
			adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}

			out, err := adapter.Run(context.Background(), RunRequest{
				Mode:           tc.mode,
				Envelope:       testEnvelope(workspace),
				Workspace:      workspace,
				CompletionPath: tc.completionPath,
				Timeout:        time.Minute,
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(out.Payload) == 0 {
				t.Fatal("recovery produced no completion payload")
			}
			if got := strings.Count(string(out.Transcript), "finished"); got != 2 {
				t.Fatalf("recovery transcript preserved %d process outputs, want 2: %q", got, out.Transcript)
			}
			if len(calls) != 2 {
				t.Fatalf("process calls = %d, want initial call plus one recovery", len(calls))
			}
			firstSession, firstOK := nativeSessionID(calls[0])
			secondSession, secondOK := nativeSessionID(calls[1])
			if !firstOK || !secondOK || firstSession != secondSession {
				t.Fatalf("recovery did not resume the initial session: first=%q second=%q", firstSession, secondSession)
			}
			promptIndex := slices.Index(calls[1].Command, defaultPromptFlag)
			if promptIndex < 0 || promptIndex+1 >= len(calls[1].Command) {
				t.Fatalf("recovery command missing prompt: %v", calls[1].Command)
			}
			recoveryPrompt := calls[1].Command[promptIndex+1]
			if !strings.Contains(recoveryPrompt, tc.completionPath) ||
				!strings.Contains(recoveryPrompt, "ended without writing the mandatory completion file") {
				t.Fatalf("recovery prompt = %q", recoveryPrompt)
			}
			if calls[1].Timeout <= 0 || calls[1].Timeout >= calls[0].Timeout {
				t.Fatalf("recovery timeout = %s, want positive remainder of %s", calls[1].Timeout, calls[0].Timeout)
			}
		})
	}
}

func TestCopilotAdapterPersistentMissingCompletionStopsAfterOneRecovery(t *testing.T) {
	workspace := t.TempDir()
	calls := 0
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(ProcessRequest) error {
			calls++
			return nil
		},
	}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}

	_, err := adapter.Run(context.Background(), RunRequest{
		Mode:           ModeInvoke,
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Timeout:        time.Minute,
	})
	if !errors.Is(err, ErrNoCompletion) {
		t.Fatalf("Run error = %v, want ErrNoCompletion", err)
	}
	if calls != 2 {
		t.Fatalf("process calls = %d, want exactly one bounded recovery", calls)
	}
}

func TestMergeProcessResultsPreservesRecoveryAndDroppedByteAccounting(t *testing.T) {
	const limit = int64(10)
	for _, tc := range []struct {
		name        string
		first       ProcessResult
		second      ProcessResult
		wantText    string
		wantDropped int64
	}{
		{
			name: "truncated initial turn",
			first: ProcessResult{
				Transcript:             append([]byte("abcdefghij"), transcriptTruncationMarker(5)...),
				TranscriptTruncated:    true,
				TranscriptDroppedBytes: 5,
			},
			second:      ProcessResult{Transcript: []byte("RECOVER")},
			wantText:    "ab\nRECOVER\n[transcript truncated: 13 bytes dropped]\n",
			wantDropped: 13,
		},
		{
			name:  "truncated recovery turn",
			first: ProcessResult{Transcript: []byte("initial")},
			second: ProcessResult{
				Transcript:             append([]byte("RECOVERY!!"), transcriptTruncationMarker(4)...),
				TranscriptTruncated:    true,
				TranscriptDroppedBytes: 4,
			},
			wantText:    "RECOVERY!!\n[transcript truncated: 11 bytes dropped]\n",
			wantDropped: 11,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeProcessResults(tc.first, tc.second, limit)
			if string(got.Transcript) != tc.wantText {
				t.Fatalf("Transcript = %q, want %q", got.Transcript, tc.wantText)
			}
			if !got.TranscriptTruncated {
				t.Fatal("TranscriptTruncated = false, want true")
			}
			if got.TranscriptDroppedBytes != tc.wantDropped {
				t.Fatalf("TranscriptDroppedBytes = %d, want %d", got.TranscriptDroppedBytes, tc.wantDropped)
			}
		})
	}
}

func TestCopilotAdapterValidatesConfigAndBuildsArguments(t *testing.T) {
	modelLister := &fakeCopilotModelLister{models: testCopilotModelList()}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, ModelLister: modelLister}
	for _, tc := range []struct {
		name    string
		model   string
		options map[string]apiextensionsv1.JSON
		wantErr string
	}{
		{name: "valid", model: "claude-sonnet-5", options: testHarnessOptions(t, map[string]interface{}{"context": "long_context", "reasoningEffort": "xhigh"})},
		{name: "available model ignores fallback", model: "claude-sonnet-5", options: testHarnessOptions(t, map[string]interface{}{fallbackToDefaultOption: true})},
		{name: "default context supported", model: "claude-sonnet-4.5", options: testHarnessOptions(t, map[string]interface{}{"context": "default"})},
		{name: "fable model supported", model: "claude-fable-5"},
		{name: "fast opus model supported", model: "claude-opus-4.8-fast"},
		{name: "opus 4.5 model supported", model: "claude-opus-4.5"},
		{name: "kimi model supported", model: "kimi-k2.7-code"},
		{name: "discovered MAI model supported", model: "mai-code-1-flash-picker"},
		{name: "newly discovered model supported", model: "future-model"},
		{name: "model absent from discovery rejected", model: "mai-code-1-flash", wantErr: "unknown model"},
		{name: "unknown model", model: "not-a-model", wantErr: "unknown model"},
		{name: "unknown option", options: testHarnessOptions(t, map[string]interface{}{"temperature": "0.2"}), wantErr: "unknown harness option"},
		{name: "fallback must be boolean", model: "not-a-model", options: testHarnessOptions(t, map[string]interface{}{fallbackToDefaultOption: "yes"}), wantErr: "must be a boolean"},
		{name: "fallback must not be null", model: "claude-sonnet-5", options: testHarnessOptions(t, map[string]interface{}{fallbackToDefaultOption: nil}), wantErr: "must be a boolean"},
		{name: "fallback requires model", options: testHarnessOptions(t, map[string]interface{}{fallbackToDefaultOption: true}), wantErr: "requires an explicit model"},
		{name: "invalid option type", model: "claude-sonnet-5", options: testHarnessOptions(t, map[string]interface{}{"context": true}), wantErr: "must be a string"},
		{name: "unknown context value", model: "claude-sonnet-5", options: testHarnessOptions(t, map[string]interface{}{"context": "extended"}), wantErr: "invalid context"},
		{name: "long context unsupported", model: "claude-sonnet-4.5", options: testHarnessOptions(t, map[string]interface{}{"context": "long_context"}), wantErr: "not supported"},
		{name: "reasoning unsupported", model: "claude-sonnet-4.5", options: testHarnessOptions(t, map[string]interface{}{"reasoningEffort": "high"}), wantErr: "not supported"},
		{name: "reasoning level unsupported", model: "claude-sonnet-4.6", options: testHarnessOptions(t, map[string]interface{}{"reasoningEffort": "xhigh"}), wantErr: "not supported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := adapter.ValidateConfig(tc.model, tc.options)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateConfig: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateConfig error = %v, want %q", err, tc.wantErr)
			}
		})
	}
	if modelLister.calls != 1 {
		t.Fatalf("model discovery calls = %d, want one cached query", modelLister.calls)
	}

	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter = &CopilotAdapter{
		Command:     []string{"copilot"},
		ExtraArgs:   []string{},
		Runner:      runner,
		ModelLister: &fakeCopilotModelLister{models: testCopilotModelList()},
	}
	req := RunRequest{
		Envelope:       testEnvelope(workspace),
		Model:          "claude-sonnet-5",
		HarnessOptions: testHarnessOptions(t, map[string]interface{}{"context": "long_context", "reasoningEffort": "xhigh"}),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
	}
	if _, err := adapter.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	command := strings.Join(runner.lastReq.Command, " ")
	for _, want := range []string{
		"--model claude-sonnet-5",
		"--context long_context",
		"--reasoning-effort xhigh",
	} {
		if !strings.Contains(command, want) {
			t.Errorf("command = %q, want %q", command, want)
		}
	}
}

func TestCopilotAdapterFallbackToDefaultWarnsAndOmitsModel(t *testing.T) {
	options := testHarnessOptions(t, map[string]interface{}{
		fallbackToDefaultOption: true,
		"context":               "default",
	})
	modelLister := &fakeCopilotModelLister{models: []CopilotModelInfo{{ID: "gpt-5.4"}}}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, ModelLister: modelLister}
	resolution, err := adapter.ResolveConfig("claude-sonnet-5", options)
	if err != nil {
		t.Fatalf("ResolveConfig: %v", err)
	}
	if resolution.Model != "" {
		t.Fatalf("resolved model = %q, want harness default", resolution.Model)
	}
	if _, ok := resolution.HarnessOptions[fallbackToDefaultOption]; ok {
		t.Fatalf("resolved options retain admission-only fallback flag: %v", resolution.HarnessOptions)
	}
	if got := string(resolution.HarnessOptions["context"].Raw); got != `"default"` {
		t.Fatalf("resolved context = %s, want default", got)
	}
	if len(resolution.Warnings) != 1 ||
		resolution.Warnings[0].Kind != ConfigWarningModelFallback ||
		!strings.Contains(resolution.Warnings[0].Message, `"claude-sonnet-5"`) {
		t.Fatalf("warnings = %+v, want one model fallback warning", resolution.Warnings)
	}

	if _, err := adapter.ResolveConfig("claude-sonnet-5", nil); err == nil ||
		!strings.Contains(err.Error(), "valid models: auto, gpt-5.4") {
		t.Fatalf("unknown model error = %v, want sorted valid-model list", err)
	}

	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter = &CopilotAdapter{
		Command:     []string{"copilot"},
		ExtraArgs:   []string{},
		Runner:      runner,
		ModelLister: modelLister,
	}
	if _, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace),
		Model:          "claude-sonnet-5",
		HarnessOptions: options,
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if slices.Contains(runner.lastReq.Command, "--model") {
		t.Fatalf("command = %q, want harness default without --model", runner.lastReq.Command)
	}
	contextIndex := slices.Index(runner.lastReq.Command, "--context")
	if contextIndex < 0 || contextIndex+1 >= len(runner.lastReq.Command) || runner.lastReq.Command[contextIndex+1] != "default" {
		t.Fatalf("command = %q, want retained --context default", runner.lastReq.Command)
	}
}

func TestCopilotAdapterTreatsPolicyDisabledModelsAsUnavailable(t *testing.T) {
	modelLister := &fakeCopilotModelLister{models: []CopilotModelInfo{
		{ID: "disabled-model", PolicyState: copilotModelPolicyDisabled},
	}}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, ModelLister: modelLister}

	_, err := adapter.ResolveConfig("disabled-model", nil)
	if err == nil || !strings.Contains(err.Error(), `unknown model "disabled-model"`) {
		t.Fatalf("strict disabled model error = %v, want unknown-model error", err)
	}
	if !strings.Contains(err.Error(), "valid models: auto") ||
		strings.Contains(err.Error(), "valid models: auto, disabled-model") {
		t.Fatalf("strict disabled model error = %v, want valid list excluding disabled model", err)
	}

	resolution, err := adapter.ResolveConfig("disabled-model", testHarnessOptions(t, map[string]interface{}{
		fallbackToDefaultOption: true,
	}))
	if err != nil {
		t.Fatalf("fallback disabled model: %v", err)
	}
	if resolution.Model != "" || len(resolution.Warnings) != 1 ||
		resolution.Warnings[0].Kind != ConfigWarningModelFallback {
		t.Fatalf("fallback disabled model resolution = %+v, want default with model-fallback warning", resolution)
	}
}

func TestCopilotAdapterTreatsPolicyDisabledAutoAsUnavailable(t *testing.T) {
	adapter := &CopilotAdapter{
		Command: []string{"copilot"},
		ModelLister: &fakeCopilotModelLister{models: []CopilotModelInfo{
			{ID: "auto", PolicyState: copilotModelPolicyDisabled},
			{ID: "gpt-5.4"},
		}},
	}

	_, err := adapter.ResolveConfig("auto", nil)
	if err == nil || !strings.Contains(err.Error(), `unknown model "auto"; valid models: gpt-5.4`) {
		t.Fatalf("strict disabled auto error = %v, want valid list excluding auto", err)
	}
	resolution, err := adapter.ResolveConfig("auto", testHarnessOptions(t, map[string]interface{}{
		fallbackToDefaultOption: true,
	}))
	if err != nil {
		t.Fatalf("fallback disabled auto: %v", err)
	}
	if resolution.Model != "" || len(resolution.Warnings) != 1 ||
		resolution.Warnings[0].Kind != ConfigWarningModelFallback {
		t.Fatalf("fallback disabled auto resolution = %+v, want harness default with warning", resolution)
	}
}

func TestCopilotSDKModelMappingPreservesPolicyState(t *testing.T) {
	got := mapCopilotModelInfo(copilotsdk.ModelInfo{
		ID:     "disabled-model",
		Policy: &copilotsdk.ModelPolicy{State: copilotModelPolicyDisabled},
	})
	if got.ID != "disabled-model" || got.PolicyState != copilotModelPolicyDisabled {
		t.Fatalf("mapped model = %+v, want disabled policy state", got)
	}
}

func TestCopilotAdapterRunsAdmittedResolutionWithoutRediscovery(t *testing.T) {
	modelLister := &fakeCopilotModelLister{responses: [][]CopilotModelInfo{
		{{ID: "gpt-5.4"}},
		{{ID: "claude-sonnet-5"}},
	}}
	admissionAdapter := &CopilotAdapter{Command: []string{"copilot"}, ModelLister: modelLister}
	resolution, err := admissionAdapter.ResolveConfig(
		"claude-sonnet-5",
		testHarnessOptions(t, map[string]interface{}{fallbackToDefaultOption: true}),
	)
	if err != nil {
		t.Fatalf("admission ResolveConfig: %v", err)
	}

	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	runtimeAdapter := &CopilotAdapter{
		Command:     []string{"copilot"},
		ExtraArgs:   []string{},
		ModelLister: modelLister,
		Runner:      runner,
	}
	if _, err := runtimeAdapter.Run(context.Background(), RunRequest{
		Envelope:              testEnvelope(workspace),
		Model:                 resolution.Model,
		HarnessOptions:        resolution.HarnessOptions,
		HarnessConfigResolved: true,
		Workspace:             workspace,
		CompletionPath:        DefaultResultPath,
	}); err != nil {
		t.Fatalf("run admitted resolution: %v", err)
	}
	if modelLister.calls != 1 {
		t.Fatalf("model discovery calls = %d, want admission query only", modelLister.calls)
	}
	if slices.Contains(runner.lastReq.Command, "--model") {
		t.Fatalf("command = %q, want admitted harness default", runner.lastReq.Command)
	}
}

func TestCopilotAdapterRunsAdmittedExplicitModelWithoutRediscovery(t *testing.T) {
	modelLister := &fakeCopilotModelLister{responses: [][]CopilotModelInfo{
		{{ID: "gpt-5.4"}},
		{{ID: "claude-sonnet-5"}},
	}}
	admissionAdapter := &CopilotAdapter{Command: []string{"copilot"}, ModelLister: modelLister}
	resolution, err := admissionAdapter.ResolveConfig("gpt-5.4", nil)
	if err != nil {
		t.Fatalf("admission ResolveConfig: %v", err)
	}

	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	runtimeAdapter := &CopilotAdapter{
		Command:     []string{"copilot"},
		ExtraArgs:   []string{},
		ModelLister: modelLister,
		Runner:      runner,
	}
	if _, err := runtimeAdapter.Run(context.Background(), RunRequest{
		Envelope:              testEnvelope(workspace),
		Model:                 resolution.Model,
		HarnessOptions:        resolution.HarnessOptions,
		HarnessConfigResolved: true,
		Workspace:             workspace,
		CompletionPath:        DefaultResultPath,
	}); err != nil {
		t.Fatalf("run admitted resolution: %v", err)
	}
	if modelLister.calls != 1 {
		t.Fatalf("model discovery calls = %d, want admission query only", modelLister.calls)
	}
	modelIndex := slices.Index(runner.lastReq.Command, "--model")
	if modelIndex < 0 || modelIndex+1 >= len(runner.lastReq.Command) ||
		runner.lastReq.Command[modelIndex+1] != "gpt-5.4" {
		t.Fatalf("command = %q, want admitted --model gpt-5.4", runner.lastReq.Command)
	}
}

// TestCopilotAdapterFailsClosedOnlyWhenTheHarnessAnswers pins the distinction
// this adapter draws between two outcomes that were originally conflated:
//
//   - the harness answered and did not offer the model -> fail closed, because
//     the catalogue is authoritative and the model is genuinely wrong;
//   - the harness could not be reached at all -> accept unverified, because
//     "cannot determine availability" is not evidence of an invalid model.
//
// The original behaviour failed closed in both cases. That made config validity
// depend on whether a Copilot CLI happened to be installed and authenticated on
// the validating machine, so `goobers validate` and the checks CI gate rejected
// configs on every runner without the CLI while accepting them on a developer
// laptop.
func TestCopilotAdapterFailsClosedOnlyWhenTheHarnessAnswers(t *testing.T) {
	t.Run("harness answered without the model", func(t *testing.T) {
		adapter := &CopilotAdapter{
			Command:     []string{"copilot"},
			ModelLister: &fakeCopilotModelLister{models: []CopilotModelInfo{{ID: "gpt-5.4"}}},
		}
		_, err := adapter.ResolveConfig("no-such-model", nil)
		if err == nil || !strings.Contains(err.Error(), `unknown model "no-such-model"`) {
			t.Fatalf("ResolveConfig error = %v, want unknown-model rejection", err)
		}
	})

	t.Run("harness unreachable", func(t *testing.T) {
		adapter := &CopilotAdapter{
			Command:     []string{"copilot"},
			ModelLister: &fakeCopilotModelLister{err: errors.New("runtime unavailable")},
		}
		resolution, err := adapter.ResolveConfig("gpt-5.4", nil)
		if err != nil {
			t.Fatalf("ResolveConfig error = %v, want the model accepted unverified", err)
		}
		if len(resolution.Warnings) != 1 ||
			resolution.Warnings[0].Kind != ConfigWarningModelUnverified ||
			!strings.Contains(resolution.Warnings[0].Message, "runtime unavailable") {
			t.Fatalf("Warnings = %+v, want one %s warning naming the discovery failure",
				resolution.Warnings, ConfigWarningModelUnverified)
		}
	})
}

func TestCopilotAdapterUndeclaredCapabilityNeverResolved(t *testing.T) {
	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CopilotAdapter{
		Command:         []string{"copilot"},
		Runner:          runner,
		EnvCapabilities: map[string]string{"repo:push": "GH_TOKEN"},
	}

	// Credentials materialized for "repo:read" only — "repo:push" was never
	// declared, so the adapter must not (and per credentials.Set, cannot)
	// resolve or inject it.
	creds := pushCredentials(t, "repo:read", "irrelevant")
	env := testEnvelope(workspace, "repo:read")
	req := RunRequest{
		Envelope:       env,
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    creds,
	}
	if _, err := adapter.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, kv := range runner.lastReq.Env {
		if strings.HasPrefix(kv, "GH_TOKEN=") {
			t.Fatalf("undeclared capability's token leaked into env: %v", runner.lastReq.Env)
		}
	}
}

// TestCopilotAdapterDoesNotPassthroughAmbientDaemonEnv is the regression test
// for the QA finding on PR #70: the subprocess must not inherit the daemon
// process's own environment wholesale (os.Environ()), since that would leak
// any resolver-sourced credential env var (e.g. instance.yaml's
// token.env — GOOBERS_GITHUB_TOKEN) into every stage regardless of whether it
// declared the corresponding capability (SEC-045, GBO-052).
func TestCopilotAdapterDoesNotPassthroughAmbientDaemonEnv(t *testing.T) {
	const ambientSecretVar = "GOOBERS_GITHUB_TOKEN"
	t.Setenv(ambientSecretVar, "ambient-daemon-secret-never-declared")

	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}

	// No capabilities declared at all — the stage asked for nothing.
	env := testEnvelope(workspace)
	req := RunRequest{
		Envelope:       env,
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    pushCredentials(t, "unused", "unused"),
	}
	if _, err := adapter.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, kv := range runner.lastReq.Env {
		if strings.HasPrefix(kv, ambientSecretVar+"=") {
			t.Fatalf("ambient daemon env var leaked into subprocess env: %v", runner.lastReq.Env)
		}
	}
	// The allowlist itself (PATH at minimum) should still be present, so the
	// fix isn't accidentally starving the CLI of what it needs to run.
	foundPath := false
	for _, kv := range runner.lastReq.Env {
		if strings.HasPrefix(kv, "PATH=") {
			foundPath = true
		}
	}
	if !foundPath {
		t.Fatalf("expected PATH to still be passed through via the allowlist, got %v", runner.lastReq.Env)
	}
}

// TestCopilotAdapterPassesThroughExtendedAllowlist is the regression test for
// #75: the well-known, non-secret env conventions diverse tier-1 hosts rely
// on (XDG base dirs, locale, TLS/proxy config) must reach the subprocess, not
// just PATH/HOME/TMPDIR.
func TestCopilotAdapterPassesThroughExtendedAllowlist(t *testing.T) {
	extended := map[string]string{
		"XDG_CONFIG_HOME": "/home/tester/.config",
		"XDG_DATA_HOME":   "/home/tester/.local/share",
		"LANG":            "en_US.UTF-8",
		"LC_ALL":          "C",
		"LC_CTYPE":        "en_US.UTF-8",
		"SSL_CERT_FILE":   "/etc/ssl/certs/custom-ca.pem",
		"HTTP_PROXY":      "http://proxy.example.internal:8080",
		"HTTPS_PROXY":     "https://proxy.example.internal:8443",
		"NO_PROXY":        "localhost,127.0.0.1",
	}
	for name, value := range extended {
		t.Setenv(name, value)
	}

	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}

	req := RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    pushCredentials(t, "unused", "unused"),
	}
	if _, err := adapter.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := make(map[string]bool, len(extended))
	for name, value := range extended {
		want := name + "=" + value
		for _, kv := range runner.lastReq.Env {
			if kv == want {
				got[name] = true
			}
		}
	}
	for name := range extended {
		if !got[name] {
			t.Fatalf("%s did not pass through into subprocess env, got %v", name, runner.lastReq.Env)
		}
	}
}

// TestCopilotAdapterExtendedAllowlistStillBlocksSecretShapedVars proves the
// #75 extension stays default-deny: an ambient var that merely resembles an
// allowlisted name (shares a prefix substring) or looks like a credential
// must not pass, only exact allowlisted names and the LC_* family do.
func TestCopilotAdapterExtendedAllowlistStillBlocksSecretShapedVars(t *testing.T) {
	blocked := map[string]string{
		// Secret-shaped, unrelated to the allowlist.
		"AWS_SECRET_ACCESS_KEY": "not-a-real-secret-but-should-never-pass",
		// Shares the "LANG" substring as a prefix but is a distinct var name —
		// would leak if baseEnv used strings.HasPrefix(name, "LANG") instead
		// of an exact match.
		"LANGUAGE_MODEL_API_KEY": "should-not-pass-either",
		// Shares "LC_" as a substring but not as a prefix — must not match
		// the LC_* family.
		"LOCALE_LC_OVERRIDE_SECRET": "should-not-pass",
	}
	for name, value := range blocked {
		t.Setenv(name, value)
	}
	t.Setenv("LANG", "en_US.UTF-8") // the real, exact allowlisted name

	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}

	req := RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    pushCredentials(t, "unused", "unused"),
	}
	if _, err := adapter.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for name := range blocked {
		for _, kv := range runner.lastReq.Env {
			if strings.HasPrefix(kv, name+"=") {
				t.Fatalf("blocked var %s leaked into subprocess env: %v", name, runner.lastReq.Env)
			}
		}
	}
	foundLang := false
	for _, kv := range runner.lastReq.Env {
		if kv == "LANG=en_US.UTF-8" {
			foundLang = true
		}
	}
	if !foundLang {
		t.Fatalf("expected the exact allowlisted LANG to still pass through, got %v", runner.lastReq.Env)
	}
}

// TestBaseEnvMatchesProcenv is the #248 drift-guard: harness's baseEnv()
// must be exactly procenv.BaseEnv() — the shared definition executor's
// baseEnv() also delegates to — not a local copy that can silently diverge
// again the way #98/#122 did.
func TestBaseEnvMatchesProcenv(t *testing.T) {
	t.Setenv("GOMODCACHE", "/custom/gomodcache")
	t.Setenv("LC_ALL", "C")

	got := append([]string(nil), baseEnv(nil)...)
	want := append([]string(nil), procenv.BaseEnv()...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("harness baseEnv() diverged from procenv.BaseEnv():\n got:  %v\n want: %v", got, want)
	}
}

// TestBaseEnvAppliesExtraAllowlist is #736's harness path: the adapter's
// ExtraEnvAllowlist (RunnerConfig.EnvPassthrough) reaches the harness
// subprocess env additively, staying default-deny for undeclared vars.
func TestBaseEnvAppliesExtraAllowlist(t *testing.T) {
	t.Setenv("MY_HARNESS_TOOLCHAIN", "/opt/harness-tool")
	t.Setenv("MY_HARNESS_UNDECLARED", "should-not-pass")

	env := baseEnv([]string{"MY_HARNESS_TOOLCHAIN"})
	found := false
	for _, kv := range env {
		if kv == "MY_HARNESS_TOOLCHAIN=/opt/harness-tool" {
			found = true
		}
		if strings.HasPrefix(kv, "MY_HARNESS_UNDECLARED=") {
			t.Fatalf("undeclared ambient var leaked into harness baseEnv: %v", env)
		}
	}
	if !found {
		t.Fatalf("extra-allowlisted var missing from harness baseEnv: %v", env)
	}
}

// TestCopilotAdapterEmptyWorkspaceIsConfigError is the regression test for
// #122: exec.Cmd treats Dir == "" as "run in the current process's working
// directory" — an unset RunRequest.Workspace must fail closed as a
// configuration error instead of silently running in the daemon's own cwd.
func TestCopilotAdapterEmptyWorkspaceIsConfigError(t *testing.T) {
	adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: &fakeProcessRunner{result: ProcessResult{ExitCode: 0}}}
	_, err := adapter.Run(context.Background(), RunRequest{CompletionPath: DefaultResultPath}) // Workspace left empty
	if err == nil {
		t.Fatal("expected an error for an empty Workspace")
	}
}

func TestCopilotAdapterFailsClosedOnMissingCommand(t *testing.T) {
	adapter := &CopilotAdapter{}
	if _, err := adapter.Preflight(context.Background()); err == nil {
		t.Fatal("expected Preflight to fail with no command configured")
	}
	_, err := adapter.Run(context.Background(), RunRequest{Workspace: t.TempDir(), CompletionPath: DefaultResultPath})
	if err == nil {
		t.Fatal("expected Run to fail with no command configured")
	}
}

func TestCopilotAdapterPreflightMissingBinary(t *testing.T) {
	adapter := &CopilotAdapter{Command: []string{"definitely-not-a-real-copilot-cli-binary"}}
	_, err := adapter.Preflight(context.Background())
	if err == nil {
		t.Fatal("expected Preflight to fail for a binary not on PATH")
	}
	if !strings.Contains(err.Error(), "not found on PATH") {
		t.Fatalf("error = %v, want an actionable PATH message", err)
	}
}

func TestCopilotAdapterPreflightSucceeds(t *testing.T) {
	runner := &fakeProcessRunner{result: ProcessResult{ExitCode: 0, Transcript: []byte("copilot version 1.2.3\n")}}
	adapter := &CopilotAdapter{Command: []string{"echo"}, Runner: runner}
	info, err := adapter.Preflight(context.Background())
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if info.Version != "copilot version 1.2.3" {
		t.Fatalf("Preflight version = %q", info.Version)
	}
}

func TestCopilotAdapterPreflightRequiresVersionOutput(t *testing.T) {
	adapter := &CopilotAdapter{
		Command: []string{"echo"},
		Runner:  &fakeProcessRunner{result: ProcessResult{ExitCode: 0}},
	}
	if _, err := adapter.Preflight(context.Background()); err == nil || !strings.Contains(err.Error(), "returned no version") {
		t.Fatalf("Preflight error = %v", err)
	}
}

func TestCopilotAdapterPreflightNonZeroExit(t *testing.T) {
	runner := &fakeProcessRunner{result: ProcessResult{ExitCode: 1}}
	adapter := &CopilotAdapter{Command: []string{"echo"}, Runner: runner}
	_, err := adapter.Preflight(context.Background())
	if err == nil {
		t.Fatal("expected Preflight to fail on non-zero exit")
	}
}

// TestCopilotAdapterRun_PassesMaxTranscriptBytesThrough confirms
// RunRequest.MaxTranscriptBytes reaches the underlying ProcessRequest, and
// Outcome carries back whatever the ProcessRunner reported — the plumbing
// #245 threads between the two layers.
func TestCopilotAdapterRun_PassesMaxTranscriptBytesThrough(t *testing.T) {
	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{TranscriptTruncated: true, TranscriptDroppedBytes: 42},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}
	out, err := adapter.Run(context.Background(), RunRequest{
		Workspace:          workspace,
		CompletionPath:     DefaultResultPath,
		MaxTranscriptBytes: 2048,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runner.lastReq.MaxTranscriptBytes != 2048 {
		t.Fatalf("ProcessRequest.MaxTranscriptBytes = %d, want 2048", runner.lastReq.MaxTranscriptBytes)
	}
	if !out.TranscriptTruncated || out.TranscriptDroppedBytes != 42 {
		t.Fatalf("Outcome = {%v, %d}, want {true, 42}", out.TranscriptTruncated, out.TranscriptDroppedBytes)
	}
}

// TestCopilotAdapterPreflightSignedOutFailsAuthProbe is the #238 control: a CLI
// that passes --version but fails the configured auth probe (signed out) fails
// preflight — the case a version-only check misses (GBO-011).
func TestCopilotAdapterPreflightSignedOutFailsAuthProbe(t *testing.T) {
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0, Transcript: []byte("copilot version 1.2.3\n")}, // --version succeeds
		act: func(req ProcessRequest) error {
			for _, a := range req.Command {
				if a == "auth" { // the auth probe fails: signed out
					return errors.New("not signed in")
				}
			}
			return nil
		},
	}
	adapter := &CopilotAdapter{Command: []string{"echo"}, AuthCheckArgs: []string{"auth", "status"}, Runner: runner}
	_, err := adapter.Preflight(context.Background())
	if err == nil {
		t.Fatal("expected preflight to fail when the sign-in probe fails")
	}
	if !strings.Contains(err.Error(), "sign") {
		t.Fatalf("error should be an actionable sign-in message: %v", err)
	}
}

func TestCopilotAdapterPreflightReportsScrubbedBoundedProbeOutput(t *testing.T) {
	secret := "ghp_" + strings.Repeat("x", 36)
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0, Transcript: []byte("copilot version 1.2.3\n")},
	}
	runner.act = func(req ProcessRequest) error {
		if slices.Contains(req.Command, "auth") {
			runner.result = ProcessResult{
				ExitCode:   1,
				Transcript: []byte("Access denied by policy settings bearer " + secret + "\n" + strings.Repeat("detail ", 1000)),
			}
			return errors.New("exit status 1")
		}
		return nil
	}

	adapter := &CopilotAdapter{Command: []string{"echo"}, AuthCheckArgs: []string{"auth", "status"}, Runner: runner}
	_, err := adapter.Preflight(context.Background())
	if err == nil {
		t.Fatal("expected auth probe failure")
	}
	message := err.Error()
	if !strings.Contains(message, "exited 1: Access denied by policy settings") {
		t.Fatalf("error omitted probe output: %v", err)
	}
	if strings.Contains(message, secret) || !strings.Contains(message, journal.Redacted) {
		t.Fatalf("error did not scrub probe output: %v", err)
	}
	if !strings.Contains(message, "truncated") || len(message) > maxPreflightDiagnosticBytes+512 {
		t.Fatalf("error was not bounded: len=%d error=%v", len(message), err)
	}
	if !strings.Contains(message, "if this is an authentication failure") {
		t.Fatalf("error asserted an authentication failure: %v", err)
	}
}

// TestCopilotAdapterPreflightSignedInPasses confirms preflight passes when both
// --version and the configured auth probe succeed.
func TestCopilotAdapterPreflightSignedInPasses(t *testing.T) {
	adapter := &CopilotAdapter{
		Command:       []string{"echo"},
		AuthCheckArgs: []string{"auth", "status"},
		Runner:        &fakeProcessRunner{result: ProcessResult{ExitCode: 0, Transcript: []byte("copilot version 1.2.3\n")}},
	}
	if _, err := adapter.Preflight(context.Background()); err != nil {
		t.Fatalf("preflight should pass when signed in: %v", err)
	}
}

// TestCopilotAdapterPreflightCarriesAmbientModelToken is the headless-PAT fix:
// Preflight has no RunRequest, so it cannot resolve the agent:model credential
// credentialEnv injects at run time — the sign-in probe would fail a valid
// headless setup whose Copilot token is supplied by env (COPILOT_GITHUB_TOKEN).
// When that token is present in the ambient environment, the probe must carry
// it so preflight reflects the same auth the run will use.
func TestCopilotAdapterPreflightCarriesAmbientModelToken(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "pat-headless-xyz")
	var authProbeEnv []string
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0, Transcript: []byte("copilot version 1.2.3\n")},
		act: func(req ProcessRequest) error {
			for _, a := range req.Command {
				if a == "auth" {
					authProbeEnv = append([]string(nil), req.Env...)
				}
			}
			return nil
		},
	}
	adapter := &CopilotAdapter{Command: []string{"echo"}, AuthCheckArgs: []string{"auth", "status"}, Runner: runner}
	if _, err := adapter.Preflight(context.Background()); err != nil {
		t.Fatalf("preflight should pass with an ambient model token: %v", err)
	}
	found := false
	for _, kv := range authProbeEnv {
		if kv == "COPILOT_GITHUB_TOKEN=pat-headless-xyz" {
			found = true
		}
	}
	if !found {
		t.Fatalf("auth probe env should carry the ambient COPILOT_GITHUB_TOKEN; got %v", authProbeEnv)
	}
}

// TestCopilotAdapterPreflightFallsBackToGHToken confirms the ambient-token probe
// also honors GH_TOKEN/GITHUB_TOKEN, the conventional fallbacks the Copilot CLI
// accepts, when COPILOT_GITHUB_TOKEN itself is unset.
func TestCopilotAdapterPreflightFallsBackToGHToken(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "gh-fallback-abc")
	var authProbeEnv []string
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0, Transcript: []byte("copilot version 1.2.3\n")},
		act: func(req ProcessRequest) error {
			for _, a := range req.Command {
				if a == "auth" {
					authProbeEnv = append([]string(nil), req.Env...)
				}
			}
			return nil
		},
	}
	adapter := &CopilotAdapter{Command: []string{"echo"}, AuthCheckArgs: []string{"auth", "status"}, Runner: runner}
	if _, err := adapter.Preflight(context.Background()); err != nil {
		t.Fatalf("preflight should pass with a GH_TOKEN fallback: %v", err)
	}
	found := false
	for _, kv := range authProbeEnv {
		if kv == "COPILOT_GITHUB_TOKEN=gh-fallback-abc" {
			found = true
		}
	}
	if !found {
		t.Fatalf("auth probe env should map GH_TOKEN into COPILOT_GITHUB_TOKEN; got %v", authProbeEnv)
	}
}

// TestCopilotAdapterPreflightNoAuthProbeByDefault confirms that with no
// AuthCheckArgs configured, preflight does not run (or require) an auth probe —
// so the version-only path is unchanged until a real auth command is wired.
func TestCopilotAdapterPreflightNoAuthProbeByDefault(t *testing.T) {
	calls := 0
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0, Transcript: []byte("copilot version 1.2.3\n")},
		act:    func(ProcessRequest) error { calls++; return nil },
	}
	adapter := &CopilotAdapter{Command: []string{"echo"}, Runner: runner} // no AuthCheckArgs
	if _, err := adapter.Preflight(context.Background()); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly one probe (--version), got %d — no auth probe should run by default", calls)
	}
}

// TestCopilotResolveConfigAcceptsModelWhenDiscoveryUnavailable covers config
// admission on a machine where the Copilot CLI cannot be reached at all — no
// binary on PATH, or no authenticated session. That is the state of every CI
// runner, and it is not reproducible on a developer machine that has the CLI
// installed, so it is asserted explicitly rather than left to integration
// coverage.
//
// An unreachable harness means "cannot determine availability", which must not
// be reported as an invalid model: otherwise a config's validity depends on
// what happens to be installed on the validating machine.
func TestCopilotResolveConfigAcceptsModelWhenDiscoveryUnavailable(t *testing.T) {
	discoveryErr := errors.New(`start Copilot model discovery: failed to start CLI server: ` +
		`exec: "copilot": executable file not found in $PATH`)
	adapter := &CopilotAdapter{
		Command:     []string{"copilot"},
		ModelLister: &fakeCopilotModelLister{err: discoveryErr},
	}

	resolution, err := adapter.ResolveConfig("gpt-5.4", nil)
	if err != nil {
		t.Fatalf("ResolveConfig with unreachable harness = %v, want nil", err)
	}
	if resolution.Model != "gpt-5.4" {
		t.Fatalf("Model = %q, want the requested model preserved", resolution.Model)
	}
	if len(resolution.Warnings) != 1 || resolution.Warnings[0].Kind != ConfigWarningModelUnverified {
		t.Fatalf("Warnings = %+v, want one %s warning", resolution.Warnings, ConfigWarningModelUnverified)
	}

	// ValidateConfig is the config-admission entry point and must agree.
	if err := adapter.ValidateConfig("gpt-5.4", nil); err != nil {
		t.Fatalf("ValidateConfig with unreachable harness = %v, want nil", err)
	}
}

// TestCopilotResolveConfigDeferredDiscoveryNeverSpawns (#3336): with
// DeferDiscovery set — the daemon's startup admission mode — a cold-cache
// resolve must not invoke the lister at all (in production that invocation
// spawns the Copilot CLI, and in a memory-capped pod the spawned children can
// OOM the daemon before any timeout fires). Models are accepted unverified; a
// cache warmed before deferral was enabled is still served.
func TestCopilotResolveConfigDeferredDiscoveryNeverSpawns(t *testing.T) {
	lister := &fakeCopilotModelLister{models: testCopilotModelList()}
	adapter := &CopilotAdapter{
		Command:        []string{"copilot"},
		ModelLister:    lister,
		DeferDiscovery: true,
	}

	for _, model := range []string{"claude-fable-5", "gpt-5.4"} {
		resolution, err := adapter.ResolveConfig(model, nil)
		if err != nil {
			t.Fatalf("ResolveConfig(%q) deferred = %v, want nil", model, err)
		}
		if resolution.Model != model {
			t.Fatalf("Model = %q, want %q preserved", resolution.Model, model)
		}
		if len(resolution.Warnings) != 1 || resolution.Warnings[0].Kind != ConfigWarningModelUnverified {
			t.Fatalf("Warnings = %+v, want one %s warning", resolution.Warnings, ConfigWarningModelUnverified)
		}
	}
	if lister.calls != 0 {
		t.Fatalf("lister.calls = %d, want 0 — deferred admission must never spawn the CLI", lister.calls)
	}

	// A warm cache is still authoritative under deferral: warm it with
	// deferral off, flip deferral on, and an unknown model must still be
	// REJECTED from the cache rather than accepted unverified.
	warm := &CopilotAdapter{Command: []string{"copilot"}, ModelLister: lister}
	if _, err := warm.ResolveConfig("claude-fable-5", nil); err != nil {
		t.Fatalf("warm-up ResolveConfig = %v, want nil", err)
	}
	if lister.calls != 1 {
		t.Fatalf("lister.calls after warm-up = %d, want 1", lister.calls)
	}
	warm.DeferDiscovery = true
	if _, err := warm.ResolveConfig("no-such-model", nil); err == nil {
		t.Fatal("ResolveConfig(no-such-model) with a warm cache = nil, want unknown-model error")
	}
	if lister.calls != 1 {
		t.Fatalf("lister.calls = %d, want 1 — the warm cache must serve without a new spawn", lister.calls)
	}
}

// TestCopilotResolveConfigFailedDiscoveryIsNotRetriedPerGoober (#3336): one
// adapter resolves every goober in an admission pass, and a failing discovery
// used to be re-attempted for each — measured in-cluster as a fresh ~295MB CLI
// process per goober, OOMing the pod. A failed discovery is negative-cached
// for the adapter's lifetime: exactly one lister call no matter how many
// goobers resolve through it.
func TestCopilotResolveConfigFailedDiscoveryIsNotRetriedPerGoober(t *testing.T) {
	lister := &fakeCopilotModelLister{err: errors.New("protocol handshake never completed")}
	adapter := &CopilotAdapter{
		Command:     []string{"copilot"},
		ModelLister: lister,
	}

	for i := 0; i < 14; i++ {
		resolution, err := adapter.ResolveConfig("claude-fable-5", nil)
		if err != nil {
			t.Fatalf("ResolveConfig #%d = %v, want nil (unverified acceptance)", i, err)
		}
		if len(resolution.Warnings) != 1 || resolution.Warnings[0].Kind != ConfigWarningModelUnverified {
			t.Fatalf("Warnings #%d = %+v, want one %s warning", i, resolution.Warnings, ConfigWarningModelUnverified)
		}
	}
	if lister.calls != 1 {
		t.Fatalf("lister.calls = %d, want exactly 1 — a failed discovery must not be retried per goober", lister.calls)
	}
}

// TestCopilotResolveConfigUnverifiedStillRejectsMalformedOptions guards the
// other half: skipping the model check must not skip option validation. Only
// the capability-dependent checks are unknowable when discovery fails; option
// shape is not.
func TestCopilotResolveConfigUnverifiedStillRejectsMalformedOptions(t *testing.T) {
	adapter := &CopilotAdapter{
		Command:     []string{"copilot"},
		ModelLister: &fakeCopilotModelLister{err: errors.New("copilot unavailable")},
	}

	if _, err := adapter.ResolveConfig("gpt-5.4", map[string]apiextensionsv1.JSON{
		"nonsense": {Raw: []byte(`"value"`)},
	}); err == nil {
		t.Fatal("unknown harness option accepted while discovery was unavailable; " +
			"shape validation must still run")
	}

	if _, err := adapter.ResolveConfig("gpt-5.4", map[string]apiextensionsv1.JSON{
		"context": {Raw: []byte(`"not_a_context"`)},
	}); err == nil {
		t.Fatal("invalid context value accepted while discovery was unavailable")
	}
}

// TestCopilotResolveConfigUnverifiedAllowsCapabilityGatedOptions pins the
// reason the unverified path uses normalizeResolvedCopilotConfig rather than
// normalizeCopilotConfig. Capabilities are unknown here, so running the
// capability checks against a zero-value struct would reject long_context and
// every reasoningEffort value — turning "cannot verify" into "invalid" through
// a different door.
func TestCopilotResolveConfigUnverifiedAllowsCapabilityGatedOptions(t *testing.T) {
	adapter := &CopilotAdapter{
		Command:     []string{"copilot"},
		ModelLister: &fakeCopilotModelLister{err: errors.New("copilot unavailable")},
	}

	for _, option := range []struct {
		name  string
		value map[string]apiextensionsv1.JSON
	}{
		{"long context", map[string]apiextensionsv1.JSON{"context": {Raw: []byte(`"long_context"`)}}},
		{"reasoning effort", map[string]apiextensionsv1.JSON{"reasoningEffort": {Raw: []byte(`"high"`)}}},
	} {
		t.Run(option.name, func(t *testing.T) {
			if _, err := adapter.ResolveConfig("gpt-5.4", option.value); err != nil {
				t.Fatalf("ResolveConfig = %v, want nil (capability unknown, not unsupported)", err)
			}
		})
	}
}
