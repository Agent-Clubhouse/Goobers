package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/chat"
	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

// CodexChatConfig binds trusted invocation authority to one native session.
// StateDir is an absolute, host-owned per-session directory, retained for
// native resume and removed by the host on disposal. An empty StateDir uses
// temporary state which is removed only after writer quiescence is proven.
type CodexChatConfig struct {
	Adapter  *CodexAdapter
	Request  RunRequest
	StateDir string
}

type preparedCodexChat struct {
	argv, env []string
	workspace string
	home      string
	prompt    string
	model     string
	effort    string
	ambient   bool
	mcp       map[string]any
	cleanup   func()
}

func prepareCodexChat(ctx context.Context, cfg CodexChatConfig, o chat.StartOptions) (p preparedCodexChat, err error) {
	c := cfg.Adapter
	if c == nil || len(c.Command) == 0 {
		return p, errors.New("codex chat requires a configured adapter")
	}
	req := cfg.Request
	if req.Workspace == "" {
		req.Workspace = o.Workspace
	}
	if !filepath.IsAbs(req.Workspace) || (o.Workspace != "" && filepath.Clean(o.Workspace) != filepath.Clean(req.Workspace)) {
		return p, errors.New("codex chat workspace must match the trusted absolute invocation workspace")
	}
	if len(req.Tools) != 0 {
		return p, errors.New("codex chat does not support a restrictive built-in tool allowlist")
	}
	if o.Model != "" {
		req.Model = o.Model
	}
	options, err := normalizeCodexConfig(req.Model, req.HarnessOptions)
	if err != nil {
		return p, err
	}
	root, cleanup, err := codexChatState(cfg.StateDir, c.EphemeralTmpRoot)
	if err != nil {
		return p, err
	}
	p.cleanup = cleanup
	defer func() {
		if err != nil {
			cleanup()
		}
	}()
	p.home = filepath.Join(root, "home")
	tmp := filepath.Join(root, "tmp")
	for _, dir := range []string{p.home, tmp} {
		if err := privateCodexDirectory(dir); err != nil {
			return p, err
		}
	}
	// Never copy renewable login files. In particular, refreshing a copied
	// ChatGPT token could invalidate the operator's original login.
	if _, statErr := os.Lstat(filepath.Join(p.home, "auth.json")); !errors.Is(statErr, os.ErrNotExist) {
		return p, errors.New("codex chat private home must not contain auth.json; use API-key environment or OS keyring authentication")
	}
	req = withAutoGoobersIOCodex(req, c.SelfBin)
	env, err := buildCredentialEnv(ctx, credentialEnvConfig{
		adapterName: c.Name(), envCapabilities: c.invocationEnvCapabilities(options.auth),
		optionalCredentialCapabilities: c.OptionalCredentialCapabilities,
		extraEnvAllowlist:              c.ExtraEnvAllowlist, envUnset: c.EnvUnset,
		instanceRoot: c.InstanceRoot, selfBin: c.SelfBin,
	}, req)
	if err != nil {
		return p, err
	}
	env, err = c.configureAuthEnvironment(ctx, env, options)
	if err != nil {
		return p, err
	}
	reserved := []string{codexModelEnv, "CODEX_HOME", "TMPDIR", "TEMP", "TMP"}
	for _, name := range c.EnvCapabilities {
		reserved = append(reserved, name)
	}
	p.mcp = map[string]any{}
	env, secrets, _, err := prepareCodexMCPWithChat(ctx, req, p.home, c.SelfBin, env, reserved, true, p.mcp)
	if err != nil {
		return p, err
	}
	env = overrideEnv(env, "CODEX_HOME", p.home)
	for _, name := range []string{"TMPDIR", "TEMP", "TMP"} {
		env = overrideEnv(env, name, tmp)
	}
	shell := codexExecutionContextShellEnvironment(ctx, req, codexShellEnvironment(env, append(reserved, secrets...)), c.InstanceRoot)
	p.argv = append(resolveStdioHarnessCommand(c.Command), "app-server", "--listen", "stdio://", "--disable", "hooks")
	for _, override := range []string{
		`projects.` + tomlQuote(filepath.Clean(req.Workspace)) + `.trust_level="untrusted"`,
		"project_root_markers=[]", "project_doc_max_bytes=0", "project_doc_fallback_filenames=[]",
		`web_search="disabled"`, "sandbox_workspace_write.network_access=false",
		`shell_environment_policy.inherit="none"`, "shell_environment_policy.ignore_default_excludes=false",
		"shell_environment_policy.set=" + tomlStringMap(shell),
		`cli_auth_credentials_store="keyring"`,
	} {
		p.argv = append(p.argv, "-c", override)
	}
	if req.Sandbox != nil {
		roots, err := gitWritableRoots(req.Workspace)
		if err != nil {
			return p, err
		}
		p.argv, _, err = confineArgv(req.Sandbox, p.argv, req.Workspace, append(roots, root))
		if err != nil {
			return p, err
		}
	}
	p.env, p.workspace, p.model, p.effort = env, req.Workspace, req.Model, options.effort
	p.ambient = options.auth == CodexAuthAmbientChatGPT
	p.prompt = renderPrompt(req)
	if o.SystemPrompt != "" {
		p.prompt += "\n\n" + o.SystemPrompt
	}
	return p, nil
}

func codexChatState(state, tempRoot string) (string, func(), error) {
	temporary := state == ""
	if temporary {
		var err error
		state, err = os.MkdirTemp(tempRoot, "goobers-codex-chat-*")
		if err != nil {
			return "", nil, err
		}
	} else if !filepath.IsAbs(state) {
		return "", nil, errors.New("codex chat StateDir must be absolute")
	}
	if err := privateCodexDirectory(state); err != nil {
		if temporary {
			_ = os.RemoveAll(state)
		}
		return "", nil, err
	}
	lock, err := platformlock.TryAcquire(filepath.Join(state, "session.lock"))
	if err != nil {
		if temporary {
			_ = os.RemoveAll(state)
		}
		return "", nil, fmt.Errorf("codex chat state is already in use: %w", err)
	}
	return state, func() {
		_ = lock.Release()
		if temporary {
			_ = os.RemoveAll(state)
		}
	}, nil
}

func privateCodexDirectory(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("codex chat state must be a private directory")
	}
	return os.Chmod(path, 0o700)
}

func codexChatStdioServer(command string, args, envNames, tools []string) map[string]any {
	if args == nil {
		args = []string{}
	}
	if envNames == nil {
		envNames = []string{}
	}
	server := map[string]any{"command": command, "args": args, "env_vars": envNames, "required": true, "enabled": true}
	if len(tools) > 0 {
		server["enabled_tools"] = tools
	}
	return server
}
