package credentials

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type trustedLocalProcessKey struct{}

// TrustedLocalAuthentication is the selected parent's ambient harness login.
// Home names that harness's config directory, not the OS user's entire home.
// The daemon derives this from retained parent policy; authored inputs cannot
// select another harness, authentication directory or file-backed mode.
type TrustedLocalAuthentication struct {
	Harness         string
	Home            string
	AllowFileBacked bool
}

type trustedLocalProcess struct {
	home string
	auth TrustedLocalAuthentication
}

// WithTrustedLocalProcess authorizes an admitted host-process invocation. The
// daemon owns home and must retain it until every process writer has joined.
// Neither a workflow envelope nor a process environment can grant this marker.
// This is shared-account execution, not isolation from hostile same-user code.
func WithTrustedLocalProcess(ctx context.Context, home string, authentication ...TrustedLocalAuthentication) (context.Context, error) {
	ceiling, ok := ChildCeilingFromContext(ctx)
	if !ok || !filepath.IsAbs(home) {
		return nil, errors.New("credentials: trusted-local process requires a ceiling and private home")
	}
	info, err := os.Lstat(home)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("credentials: trusted-local home is not an owned directory")
	}
	var auth TrustedLocalAuthentication
	if len(authentication) > 1 {
		return nil, ErrChildAuthenticationIsolation
	}
	if len(authentication) == 1 {
		auth = authentication[0]
		if !filepath.IsAbs(auth.Home) || !slices.Contains(ceiling.AllowedKeys, "agent:model") ||
			!slices.Contains([]string{"copilot", "claude-code", "codex"}, auth.Harness) {
			return nil, ErrChildAuthenticationIsolation
		}
	}
	// Publication authority belongs to its canonical host owner. Authored
	// processes receive only delegated model credentials, even when their
	// enclosing child also has publication permission.
	ctx, err = WithChildCeiling(ctx, ceiling.ModelOnly())
	if err != nil {
		return nil, err
	}
	return context.WithValue(ctx, trustedLocalProcessKey{}, trustedLocalProcess{home: home, auth: auth}), nil
}

// TrustedLocalProcess reports host-owned launch authority, never DSL policy.
func TrustedLocalProcess(ctx context.Context) bool {
	_, ok := ctx.Value(trustedLocalProcessKey{}).(trustedLocalProcess)
	return ok
}

// TrustedLocalAuthenticationFor returns only the selected harness's delegation.
func TrustedLocalAuthenticationFor(ctx context.Context, harness string) (TrustedLocalAuthentication, bool) {
	process, ok := ctx.Value(trustedLocalProcessKey{}).(trustedLocalProcess)
	return process.auth, ok && process.auth.Harness == harness
}

// TrustedLocalBaseEnvironment retains only OS process-launch prerequisites.
// The ordinary toolchain allowlist includes user config paths and proxy URLs;
// those can carry authentication and must not bypass the child ceiling.
func TrustedLocalBaseEnvironment(ctx context.Context, env []string) []string {
	if !TrustedLocalProcess(ctx) {
		return env
	}
	var out []string
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(key) {
		case "PATH", "SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT", "SYSTEMDRIVE",
			"PROGRAMFILES", "PROGRAMFILES(X86)", "PROGRAMW6432", "LANG":
			out = append(out, entry)
		}
	}
	return TrustedLocalEnvironment(ctx, out)
}

// TrustedLocalEnvironment redirects authentication discovery after environment
// assembly. Callers must also omit configurable ambient passthrough and only
// materialize credentials through ChildCeiling. Explicit stage variables cannot
// restore HOME, Git helpers, config overrides or credential-agent sockets.
func TrustedLocalEnvironment(ctx context.Context, env []string) []string {
	process, ok := ctx.Value(trustedLocalProcessKey{}).(trustedLocalProcess)
	if !ok {
		return env
	}
	home := process.home
	out := make([]string, 0, len(env)+12)
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		key = strings.ToUpper(key)
		switch key {
		case "HOME", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA",
			"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME",
			"CLAUDE_CONFIG_DIR", "CODEX_HOME", "COPILOT_HOME", "GH_CONFIG_DIR", "AZURE_CONFIG_DIR",
			"SSH_AUTH_SOCK", "SSH_AGENT_PID":
			continue
		}
		if strings.HasPrefix(key, "GIT_") {
			continue
		}
		out = append(out, entry)
	}
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA",
		"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME",
		"CLAUDE_CONFIG_DIR", "CODEX_HOME", "COPILOT_HOME", "GH_CONFIG_DIR", "AZURE_CONFIG_DIR"} {
		out = append(out, key+"="+home)
	}
	return append(out, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=",
		"GIT_TERMINAL_PROMPT=0")
}
