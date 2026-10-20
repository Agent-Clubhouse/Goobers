package harness

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/goobers/goobers/internal/credentials"
)

func trustedLocalTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, err := credentials.WithChildCeiling(t.Context(), credentials.NewChildCeiling(false, []string{"agent:model"}, []string{"agent:model"}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = credentials.WithTrustedLocalProcess(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func TestTrustedLocalDelegatesOnlySelectedParentAmbientLogin(t *testing.T) {
	for _, selected := range []string{"claude-code", "codex", "copilot"} {
		t.Run(selected, func(t *testing.T) {
			source := t.TempDir()
			home := t.TempDir()
			ctx, err := credentials.WithTrustedLocalProcess(trustedLocalTestContext(t), home,
				credentials.TrustedLocalAuthentication{Harness: selected, Home: source})
			if err != nil {
				t.Fatal(err)
			}
			for _, other := range []string{"claude-code", "codex", "copilot"} {
				_, ok := credentials.TrustedLocalAuthenticationFor(ctx, other)
				if ok != (selected == other) {
					t.Fatalf("delegated %s login to %s", selected, other)
				}
			}
			switch selected {
			case "claude-code":
				stubClaudeCredentialsHome(t)
				data := []byte(`{"claudeAiOauth":{"accessToken":"selected-parent-login"}}`)
				if err := os.WriteFile(filepath.Join(source, ".credentials.json"), data, 0o600); err != nil {
					t.Fatal(err)
				}
				keychain := func(context.Context, string) ([]byte, error) {
					t.Fatal("selected file login fell back to keychain")
					return nil, nil
				}
				if err := seedClaudeCredentialsForPlatform(ctx, []string{"HOME=" + home}, home, "darwin", keychain); err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(filepath.Join(home, ".credentials.json"))
				if err != nil || string(got) != string(data) {
					t.Fatalf("delegated login not copied: %v", err)
				}
			case "codex":
				adapter := CodexAdapter{}
				options := codexConfig{auth: CodexAuthAmbientChatGPT}
				env, err := adapter.configureAuthEnvironment(ctx, []string{"CODEX_API_KEY=must-not-use"}, options)
				if err != nil || !slices.Contains(env, "CODEX_HOME="+source) || slices.Contains(env, "CODEX_API_KEY=must-not-use") {
					t.Fatalf("selected Codex login: %v %v", env, err)
				}
				options.allowFileBackedCredentials = true
				if _, err := adapter.configureAuthEnvironment(ctx, nil, options); !errors.Is(err, credentials.ErrChildAuthenticationIsolation) {
					t.Fatal("keyring parent delegated file-backed tokens")
				}
			case "copilot":
				data := []byte(`{"oauthToken":"selected-parent-login"}`)
				if err := os.WriteFile(filepath.Join(source, "config.json"), data, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(source, "mcp-config.json"), []byte(`{"secret":"other-credential"}`), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := seedTrustedLocalCopilotAuthentication(ctx, []string{"COPILOT_HOME=" + home}); err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(filepath.Join(home, "config.json"))
				if err != nil || string(got) != string(data) {
					t.Fatalf("delegated login not copied: %v", err)
				}
				if _, err := os.Stat(filepath.Join(home, "mcp-config.json")); !os.IsNotExist(err) {
					t.Fatal("unselected MCP credentials copied")
				}
			}
		})
	}
}

func TestTrustedLocalCopilotExplicitKeyNeverCopiesStoredLogin(t *testing.T) {
	ctx := trustedLocalTestContext(t)
	home := t.TempDir()
	if err := seedTrustedLocalCopilotAuthentication(ctx, []string{"COPILOT_HOME=" + home}); !errors.Is(err, credentials.ErrChildAuthenticationIsolation) {
		t.Fatal("explicit-key parent allowed stored login fallback")
	}
	if err := seedTrustedLocalCopilotAuthentication(ctx, []string{"COPILOT_HOME=" + home, "COPILOT_GITHUB_TOKEN=delegated-key"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "config.json")); !os.IsNotExist(err) {
		t.Fatal("explicit credential seeded ambient config")
	}
}

func TestTrustedLocalClaudeNeverReadsStoredLogin(t *testing.T) {
	ctx := trustedLocalTestContext(t)
	keychain := func(context.Context, string) ([]byte, error) {
		t.Fatal("trusted-local invocation read the host keychain")
		return nil, nil
	}
	for _, goos := range []string{"darwin", "linux", "windows"} {
		err := seedClaudeCredentialsForPlatform(ctx, []string{"HOME=" + t.TempDir()}, t.TempDir(), goos, keychain)
		if !errors.Is(err, credentials.ErrChildAuthenticationIsolation) {
			t.Fatalf("%s stored login: %v", goos, err)
		}
		if err := seedClaudeCredentialsForPlatform(ctx, []string{"ANTHROPIC_API_KEY=sk-ant-explicit"}, t.TempDir(), goos, keychain); err != nil {
			t.Fatalf("%s delegated API key: %v", goos, err)
		}
	}
}

func TestTrustedLocalCodexRequiresDelegatedCredential(t *testing.T) {
	adapter := CodexAdapter{ModelCredential: func(context.Context) (string, error) {
		t.Fatal("trusted-local invocation used adapter fallback credentials")
		return "", nil
	}}
	ctx := trustedLocalTestContext(t)
	if _, err := adapter.configureAuthEnvironment(ctx, nil, codexConfig{}); err == nil {
		t.Fatal("missing delegated key accepted")
	}
	if _, err := adapter.configureAuthEnvironment(ctx, nil, codexConfig{auth: CodexAuthAmbientChatGPT}); !errors.Is(err, credentials.ErrChildAuthenticationIsolation) {
		t.Fatalf("ambient login accepted: %v", err)
	}
	key := "CODEX_API_KEY=sk-explicit-delegated-key"
	if env, err := adapter.configureAuthEnvironment(ctx, []string{key}, codexConfig{}); err != nil || !slices.Contains(env, key) {
		t.Fatalf("delegated key refused: %v", err)
	}
}

// Regression: discovery used to run before credentialEnv refused an
// undelegated launch, leaking both ambient tokens and adapter-wide grants.
func TestTrustedLocalRunDoesNotDiscoverWithAmbientToken(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "undelegated-ambient-token")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	lister := &fakeCopilotModelLister{models: testCopilotModelList()}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, ModelLister: lister}
	_, err := adapter.Run(trustedLocalTestContext(t), RunRequest{Workspace: t.TempDir(), Model: "requested-model"})
	if !errors.Is(err, credentials.ErrChildAuthenticationIsolation) || lister.calls != 0 {
		t.Fatalf("unresolved child configuration reached discovery: calls=%d err=%v", lister.calls, err)
	}
}

func TestTrustedLocalRunDoesNotResolveAdapterFallback(t *testing.T) {
	lister := &fakeCopilotModelLister{models: testCopilotModelList()}
	called := false
	adapter := &CopilotAdapter{Command: []string{"copilot"}, ModelLister: lister,
		ModelCredential: func(context.Context) (string, error) { called = true; return "undelegated-adapter-token", nil }}
	_, err := adapter.Run(trustedLocalTestContext(t), RunRequest{Workspace: t.TempDir(), Model: "requested-model"})
	if !errors.Is(err, credentials.ErrChildAuthenticationIsolation) || called || lister.calls != 0 {
		t.Fatalf("unresolved child configuration consulted adapter credentials: resolver=%v lister=%d err=%v", called, lister.calls, err)
	}
}

func TestTrustedLocalOfflineConfigDoesNotProbeLauncher(t *testing.T) {
	adapter := &CopilotAdapter{Command: []string{"wrapper"}, RequireLauncherContract: true, DeferDiscovery: true,
		Runner: &fakeProcessRunner{act: func(ProcessRequest) error {
			t.Fatal("offline configuration launched a wrapper")
			return nil
		}},
		ModelCredential: func(context.Context) (string, error) {
			t.Fatal("offline configuration resolved credentials")
			return "", nil
		}}
	if _, err := adapter.resolveConfig(t.Context(), "requested-model", nil); err != nil {
		t.Fatal(err)
	}
}

func TestTrustedLocalLauncherProbeUsesDelegatedEnvironment(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "unselected-host-token")
	t.Setenv("GITHUB_TOKEN", "unselected-provider-token")
	t.Setenv("EXTRA_SECRET", "unselected-secret")
	ctx := trustedLocalTestContext(t)
	calls := 0
	adapter := &CopilotAdapter{Command: []string{"wrapper"}, RequireLauncherContract: true,
		ModelCredential: func(context.Context) (string, error) {
			t.Fatal("launcher resolved adapter fallback")
			return "", nil
		},
		Runner: launcherProcessRunner(func(_ context.Context, req ProcessRequest) (ProcessResult, error) {
			calls++
			if !slices.Contains(req.Env, "COPILOT_GITHUB_TOKEN=selected-parent-token") {
				t.Fatal("launcher lost the selected model credential")
			}
			for _, key := range []string{"GITHUB_TOKEN", "EXTRA_SECRET"} {
				if _, found := environmentValue(req.Env, key); found {
					t.Fatalf("launcher inherited %s", key)
				}
			}
			if home, _ := environmentValue(req.Env, "HOME"); home == "" || home == os.Getenv("HOME") {
				t.Fatal("launcher did not receive a private home")
			}
			_, err := io.WriteString(req.StdoutCapture, `{"version":1,"sessionMode":"adapter-managed"}`)
			return ProcessResult{}, err
		})}
	if _, err := adapter.launcherSessionContract(ctx, []string{"COPILOT_GITHUB_TOKEN=selected-parent-token"}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("launcher probes=%d", calls)
	}
}

func TestTrustedLocalRefusesUnscopedProbesBeforeCredentialsOrProcesses(t *testing.T) {
	ctx := trustedLocalTestContext(t)
	runner := &fakeProcessRunner{act: func(ProcessRequest) error { t.Fatal("unscoped child probe started process"); return nil }}
	resolver := func(context.Context) (string, error) {
		t.Fatal("child probe consulted adapter-wide credential")
		return "", nil
	}
	copilot := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner, ModelCredential: resolver,
		ModelLister: &fakeCopilotModelLister{models: testCopilotModelList()}, RequireLauncherContract: true, DisableUsageOutput: true}
	claude := &ClaudeAdapter{Command: []string{"claude"}, Runner: runner, ModelCredential: resolver}
	codex := &CodexAdapter{Command: []string{"codex"}, Runner: runner, ModelCredential: resolver}
	probes := map[string]func() error{
		"copilot preflight":               func() error { _, err := copilot.Preflight(ctx); return err },
		"claude preflight":                func() error { _, err := claude.Preflight(ctx); return err },
		"codex preflight":                 func() error { _, err := codex.Preflight(ctx); return err },
		"codex explicit config preflight": func() error { _, err := codex.PreflightConfig(ctx, "", nil); return err },
		"codex undelegated ambient preflight": func() error {
			_, err := codex.PreflightConfig(ctx, "", testHarnessOptions(t, map[string]interface{}{"auth": "ambient-chatgpt"}))
			return err
		},
		"discovery":      func() error { _, err := copilot.discoverModels(ctx); return err },
		"token":          func() error { _, err := copilot.copilotModelToken(ctx); return err },
		"claude token":   func() error { _, err := claude.preflightCredentialEnv(ctx, nil); return err },
		"launcher":       func() error { _, err := copilot.launcherSessionContract(ctx); return err },
		"auth lifecycle": func() error { _, _, err := copilot.AuthCommand(ctx, "login"); return err },
	}
	// An existing executable ensures refusals are credential checks, not
	// accidental binary-not-found errors (the runner never launches it).
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	codex.Command = []string{executable}
	for name, probe := range probes {
		t.Run(name, func(t *testing.T) {
			if err := probe(); !errors.Is(err, credentials.ErrChildAuthenticationIsolation) {
				t.Fatalf("probe refusal=%v", err)
			}
		})
	}
	copilot.verifyLauncherUsageOutput(ctx, "1.0.81-1", []string{"--version"})
	// Even a populated shared discovery cache cannot resolve child config.
	copilot.availableModels = map[string]copilotModelCapabilities{"requested-model": {}}
	if _, err := copilot.discoverModels(ctx); !errors.Is(err, credentials.ErrChildAuthenticationIsolation) {
		t.Fatalf("cached discovery bypass: %v", err)
	}
	copilot.cacheLauncherContract(launcherContract{Version: 1, SessionMode: "adapter-managed"})
	if _, err := copilot.launcherSessionContract(ctx); err != nil {
		t.Fatalf("pinned launcher contract refused: %v", err)
	}
}

func TestTrustedLocalCodexAmbientPreflightUsesDelegatedLoginOnly(t *testing.T) {
	source, home := t.TempDir(), t.TempDir()
	ctx, err := credentials.WithTrustedLocalProcess(trustedLocalTestContext(t), home, credentials.TrustedLocalAuthentication{Harness: "codex", Home: source})
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("CODEX_API_KEY", "sk-undelegated-ambient-key")
	t.Setenv("EXTRA_TOKEN", "undelegated-extra-token")
	runner := &ambientCodexRunner{login: ProcessResult{Transcript: []byte("Logged in using ChatGPT\n")}}
	adapter := &CodexAdapter{Command: []string{executable}, Runner: runner, ExtraEnvAllowlist: []string{"EXTRA_TOKEN"}, ModelCredential: func(context.Context) (string, error) {
		t.Fatal("ambient child resolved explicit adapter grant")
		return "", nil
	}}
	_, err = adapter.PreflightConfig(ctx, "", testHarnessOptions(t, map[string]interface{}{"auth": "ambient-chatgpt"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.requests) != 2 {
		t.Fatalf("preflight requests=%d", len(runner.requests))
	}
	for _, req := range runner.requests {
		if !slices.Contains(req.Env, "CODEX_HOME="+source) || !slices.Contains(req.Env, "HOME="+home) {
			t.Fatalf("probe did not retain delegated homes: %v", req.Env)
		}
		_, hasKey := environmentValue(req.Env, "CODEX_API_KEY")
		_, hasExtra := environmentValue(req.Env, "EXTRA_TOKEN")
		if hasKey || hasExtra {
			t.Fatal("probe inherited undelegated credentials")
		}
	}
}
