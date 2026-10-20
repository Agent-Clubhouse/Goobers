package harness

import (
	"context"
	"errors"
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
