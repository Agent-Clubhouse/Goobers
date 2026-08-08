package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/telemetry"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

var defaultClaudeExtraArgs = []string{
	"--output-format", "stream-json",
	"--verbose",
	"--permission-mode", "bypassPermissions",
}

// ClaudeAdapter drives Claude Code in non-interactive print mode.
type ClaudeAdapter struct {
	Command                        []string
	ExtraArgs                      []string
	EnvCapabilities                map[string]string
	OptionalCredentialCapabilities map[string]bool
	Runner                         ProcessRunner
	ExtraEnvAllowlist              []string
	InstanceRoot                   string
	SelfBin                        string
}

// Name returns the adapter's diagnostic identity.
func (c *ClaudeAdapter) Name() string { return "claude-code" }

// ValidateConfig checks Claude Code model and harness option values.
func (c *ClaudeAdapter) ValidateConfig(model string, options map[string]apiextensionsv1.JSON) error {
	_, err := normalizeClaudeConfig(model, options)
	return err
}

func normalizeClaudeConfig(model string, options map[string]apiextensionsv1.JSON) (map[string]string, error) {
	if model != strings.TrimSpace(model) {
		return nil, fmt.Errorf("model must not have leading or trailing whitespace")
	}
	names := make([]string, 0, len(options))
	for name := range options {
		names = append(names, name)
	}
	sort.Strings(names)
	normalized := make(map[string]string, len(options))
	for _, name := range names {
		if name != "effort" {
			return nil, fmt.Errorf("unknown harness option %q", name)
		}
		var value string
		if err := json.Unmarshal(options[name].Raw, &value); err != nil {
			return nil, fmt.Errorf("harness option %q must be a string: %w", name, err)
		}
		switch value {
		case "low", "medium", "high", "xhigh", "max":
		default:
			return nil, fmt.Errorf("invalid effort value %q", value)
		}
		normalized[name] = value
	}
	return normalized, nil
}

// Preflight verifies that Claude Code is installed and authenticated.
func (c *ClaudeAdapter) Preflight(ctx context.Context) (PreflightInfo, error) {
	if len(c.Command) == 0 {
		return PreflightInfo{}, fmt.Errorf("harness: claude-code: no command configured")
	}
	bin := c.Command[0]
	if _, err := exec.LookPath(bin); err != nil {
		return PreflightInfo{}, fmt.Errorf("harness: claude-code: %q not found on PATH — install Claude Code and sign in before running agentic stages", bin)
	}
	env := baseEnv(c.ExtraEnvAllowlist)
	baseCommand := resolveHarnessCommand(c.Command)
	versionCommand := append(append([]string(nil), baseCommand...), "--version")
	versionProbe := fmt.Sprintf("harness: claude-code: %q --version", bin)
	res, err := c.runner().Run(ctx, ProcessRequest{
		Command:            versionCommand,
		Env:                env,
		MaxTranscriptBytes: maxPreflightDiagnosticBytes,
	})
	if err != nil || res.ExitCode != 0 {
		return PreflightInfo{}, preflightProbeError(versionProbe, res, err, "check that the CLI is installed and authenticated")
	}
	version := firstOutputLine(res.Transcript)
	if version == "" {
		return PreflightInfo{}, fmt.Errorf("harness: claude-code: %q --version returned no version", bin)
	}

	authCommand := append(append([]string(nil), baseCommand...), "auth", "status")
	authProbe := fmt.Sprintf("harness: claude-code: %q auth status", bin)
	res, err = c.runner().Run(ctx, ProcessRequest{
		Command:            authCommand,
		Env:                env,
		MaxTranscriptBytes: maxPreflightDiagnosticBytes,
	})
	if err != nil || res.ExitCode != 0 {
		return PreflightInfo{}, preflightProbeError(authProbe, res, err, "if this is an authentication failure, run `claude auth login`")
	}
	return PreflightInfo{Version: version}, nil
}

func (c *ClaudeAdapter) runner() ProcessRunner {
	if c.Runner != nil {
		return c.Runner
	}
	return ExecProcessRunner{}
}

// Run executes one non-interactive Claude Code session.
func (c *ClaudeAdapter) Run(ctx context.Context, req RunRequest) (Outcome, error) {
	if len(c.Command) == 0 {
		return Outcome{}, fmt.Errorf("harness: claude-code: no command configured")
	}
	if req.Workspace == "" {
		return Outcome{}, fmt.Errorf("harness: claude-code: RunRequest.Workspace is empty")
	}
	options, err := normalizeClaudeConfig(req.Model, req.HarnessOptions)
	if err != nil {
		return Outcome{}, fmt.Errorf("harness: claude-code: invalid configuration: %w", err)
	}

	prompt := renderPrompt(req)
	debugPath := filepath.Join(req.Workspace, ".goobers", "prompt.md")
	if err := os.MkdirAll(filepath.Dir(debugPath), 0o755); err != nil {
		return Outcome{}, fmt.Errorf("harness: claude-code: prepare prompt dir: %w", err)
	}
	if err := os.WriteFile(debugPath, []byte(prompt), 0o644); err != nil {
		return Outcome{}, fmt.Errorf("harness: claude-code: write prompt: %w", err)
	}

	baseCommand := resolveHarnessCommand(c.Command)
	argv := append(baseCommand, "-p", prompt)
	promptArg := len(baseCommand) + 1
	extra := c.ExtraArgs
	if extra == nil {
		extra = defaultClaudeExtraArgs
	}
	argv = append(argv, extra...)
	if req.Model != "" {
		argv = append(argv, "--model", req.Model)
	}
	if effort, ok := options["effort"]; ok {
		argv = append(argv, "--effort", effort)
	}
	sessionID, err := newHarnessSessionID()
	if err != nil {
		return Outcome{}, fmt.Errorf("harness: claude-code: create session id: %w", err)
	}
	sessionSelectorArg := len(argv)
	argv = append(argv, "--session-id", sessionID)

	env, err := buildCredentialEnv(ctx, credentialEnvConfig{
		adapterName:                    c.Name(),
		envCapabilities:                c.EnvCapabilities,
		optionalCredentialCapabilities: c.OptionalCredentialCapabilities,
		extraEnvAllowlist:              c.ExtraEnvAllowlist,
		instanceRoot:                   c.InstanceRoot,
		selfBin:                        c.SelfBin,
	}, req)
	if err != nil {
		return Outcome{}, err
	}

	var writableRoots []string
	if req.Sandbox != nil {
		configDir, tempDir, err := prepareClaudeSandboxRuntime(req.Workspace)
		if err != nil {
			return Outcome{}, fmt.Errorf("harness: claude-code: sandbox: %w", err)
		}
		if err := seedClaudeCredentials(ctx, env, configDir); err != nil {
			return Outcome{}, fmt.Errorf("harness: claude-code: sandbox: %w", err)
		}
		writableRoots, err = gitWritableRoots(req.Workspace)
		if err != nil {
			return Outcome{}, fmt.Errorf("harness: claude-code: sandbox: %w", err)
		}
		env = overrideEnv(env, "CLAUDE_CONFIG_DIR", configDir)
		env = overrideEnv(env, "TMPDIR", tempDir)
		wrapped, shift, err := confineArgv(req.Sandbox, argv, req.Workspace, writableRoots)
		if err != nil {
			return Outcome{}, fmt.Errorf("harness: claude-code: sandbox: %w", err)
		}
		argv = wrapped
		promptArg += shift
		sessionSelectorArg += shift
	}

	runner := c.runner()
	started := time.Now()
	initialCapture := &claudeTerminalCapture{}
	captures := []*claudeTerminalCapture{initialCapture}
	result, runErr := runner.Run(ctx, ProcessRequest{
		Command:            argv,
		Dir:                req.Workspace,
		Env:                env,
		Timeout:            req.Timeout,
		MaxTranscriptBytes: req.MaxTranscriptBytes,
		StdoutCapture:      initialCapture,
	})
	invocationResults := []ProcessResult{result}
	prompts := []string{prompt}
	var payload []byte
	var completionErr error
	if runErr == nil {
		payload, completionErr = readCompletion(req.Workspace, req.CompletionPath)
		if errors.Is(completionErr, ErrNoCompletion) {
			totalTimeout := req.Timeout
			if totalTimeout <= 0 {
				totalTimeout = DefaultTimeout
			}
			remaining := totalTimeout - time.Since(started)
			if remaining <= 0 {
				runErr = fmt.Errorf("%w after %s: %s", ErrTimeout, totalTimeout, argv[0])
				completionErr = nil
			} else {
				recoveryPrompt := renderCompletionRecoveryPrompt(req)
				prompts = append(prompts, recoveryPrompt)
				recoveryArgv := append([]string(nil), argv...)
				recoveryArgv[promptArg] = recoveryPrompt
				recoveryArgv[sessionSelectorArg] = "--resume"
				recoveryCapture := &claudeTerminalCapture{}
				captures = append(captures, recoveryCapture)
				recovery, recoveryErr := runner.Run(ctx, ProcessRequest{
					Command:            recoveryArgv,
					Dir:                req.Workspace,
					Env:                env,
					Timeout:            remaining,
					MaxTranscriptBytes: req.MaxTranscriptBytes,
					StdoutCapture:      recoveryCapture,
				})
				invocationResults = append(invocationResults, recovery)
				result = mergeProcessResults(result, recovery, req.MaxTranscriptBytes)
				if recoveryErr != nil {
					runErr = recoveryErr
					completionErr = nil
				} else {
					payload, completionErr = readCompletion(req.Workspace, req.CompletionPath)
				}
			}
		}
	}

	out := Outcome{
		Transcript:             result.Transcript,
		TranscriptTruncated:    result.TranscriptTruncated,
		TranscriptDroppedBytes: result.TranscriptDroppedBytes,
	}
	if native, ok := convertClaudeStreams(claudeInvocationStreams(invocationResults, captures, req.MaxTranscriptBytes), prompts, req.MaxTranscriptBytes, result.TranscriptDroppedBytes); ok {
		out.Metrics = native.metrics
		out.ModelUsage = native.modelUsage
		if len(native.data) > 0 {
			out.Transcript = native.data
			out.TranscriptSchema = telemetry.GenAIEventSchema
			out.TranscriptTruncated = native.truncated
			out.TranscriptDroppedBytes = native.droppedBytes
		}
	}
	if runErr != nil {
		return out, runErr
	}
	if completionErr != nil {
		return out, completionErr
	}
	out.Payload = payload
	return out, nil
}

func prepareClaudeSandboxRuntime(workspace string) (string, string, error) {
	goobersDir := filepath.Join(workspace, ".goobers")
	sandboxDir := filepath.Join(goobersDir, "sandbox")
	for _, dir := range []string{goobersDir, sandboxDir} {
		if err := ensurePlainDirectory(dir); err != nil {
			return "", "", err
		}
	}
	configDir := filepath.Join(sandboxDir, "claude-config")
	tempDir := filepath.Join(sandboxDir, "tmp")
	for _, dir := range []string{configDir, tempDir} {
		if err := os.RemoveAll(dir); err != nil {
			return "", "", fmt.Errorf("reset runtime directory: %w", err)
		}
		if err := os.Mkdir(dir, 0o700); err != nil {
			return "", "", fmt.Errorf("create runtime directory: %w", err)
		}
	}
	return configDir, tempDir, nil
}

func ensurePlainDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.Mkdir(path, 0o700); err != nil {
				return fmt.Errorf("create runtime parent: %w", err)
			}
			return nil
		}
		return fmt.Errorf("inspect runtime parent: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("runtime parent %s is not a plain directory", path)
	}
	return nil
}

const claudeCodeKeychainService = "Claude Code-credentials"

type claudeKeychainReader func(context.Context, string) ([]byte, error)

func seedClaudeCredentials(ctx context.Context, env []string, destination string) error {
	return seedClaudeCredentialsForPlatform(ctx, env, destination, runtime.GOOS, readClaudeKeychainCredentials)
}

func seedClaudeCredentialsForPlatform(
	ctx context.Context,
	env []string,
	destination string,
	goos string,
	readKeychain claudeKeychainReader,
) error {
	sourceDir := ""
	var home, userProfile string
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch name {
		case "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN":
			if value != "" {
				return nil
			}
		case "CLAUDE_CONFIG_DIR":
			if value != "" {
				sourceDir = value
			}
		case "HOME":
			home = value
		case "USERPROFILE":
			userProfile = value
		}
	}
	// HOME wins when explicitly set — matching the convention used for
	// resolving Copilot's config home (copilotConfigHome). USERPROFILE is
	// the Windows-native fallback, only consulted when HOME is unset; it
	// must not unconditionally override an explicitly configured HOME
	// (e.g. from a POSIX-style toolchain shim, or a test/sandbox override),
	// which is ambient on every real Windows runner regardless of HOME.
	if home == "" {
		home = userProfile
	}
	if sourceDir == "" && home != "" {
		sourceDir = filepath.Join(home, ".claude")
	}
	var credentials []byte
	if sourceDir != "" {
		source := filepath.Join(sourceDir, ".credentials.json")
		var err error
		credentials, err = os.ReadFile(source)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("read stored credentials: %w", err)
		}
	}
	if credentials == nil {
		if goos != "darwin" {
			return nil
		}
		var err error
		credentials, err = readKeychain(ctx, claudeCodeKeychainService)
		if err != nil {
			return fmt.Errorf("read Claude Code credentials from macOS Keychain service %q: %w", claudeCodeKeychainService, err)
		}
		credentials = bytes.TrimSpace(credentials)
		if len(credentials) == 0 {
			return fmt.Errorf("macOS Keychain service %q contains empty Claude Code credentials", claudeCodeKeychainService)
		}
	}
	target := filepath.Join(destination, ".credentials.json")
	if err := os.WriteFile(target, credentials, 0o600); err != nil {
		return fmt.Errorf("copy stored credentials: %w", err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		return fmt.Errorf("secure stored credentials: %w", err)
	}
	return nil
}

func readClaudeKeychainCredentials(ctx context.Context, service string) ([]byte, error) {
	return exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-s", service, "-w").Output()
}
