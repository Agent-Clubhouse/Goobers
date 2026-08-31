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

// dropForeignAnthropicAPIKey strips an ANTHROPIC_API_KEY entry from env
// whose value isn't shaped like a real Anthropic API key (which always
// starts with "sk-ant-"). This instance's credentialGrant schema allows only
// one agent:model grant per instance, with no per-harness scoping — so a
// grant configured to back Copilot's headless model auth (a GitHub PAT,
// injected as COPILOT_GITHUB_TOKEN for that adapter) is ALSO resolved and
// injected here as ANTHROPIC_API_KEY purely because both adapters declare
// the same capability name. A GitHub PAT is never a valid Anthropic key, so
// without this guard every claude-code invocation on a mixed-harness
// instance authenticates with a guaranteed-invalid key and fails instantly
// with a 401 "Invalid API key" — surfaced nowhere but the process's own
// (otherwise-uncaptured) transcript, since seedClaudeCredentials treats any
// non-empty ANTHROPIC_API_KEY as "caller already handled auth" and skips
// seeding the user's real stored Claude Code session as a fallback. Dropping
// a foreign-shaped key here restores that fallback — the same behavior as
// if no agent:model credential were configured for this capability at all,
// which is what Copilot-only credential configuration actually means for a
// harness that isn't Copilot. A genuinely valid Anthropic key (sk-ant-...)
// configured for this capability still passes through unchanged.
func dropForeignAnthropicAPIKey(env []string) []string {
	filtered := env[:0:0]
	for _, kv := range env {
		name, value, ok := strings.Cut(kv, "=")
		if ok && name == "ANTHROPIC_API_KEY" && !strings.HasPrefix(value, "sk-ant-") {
			continue
		}
		filtered = append(filtered, kv)
	}
	return filtered
}

var defaultClaudeExtraArgs = []string{
	"--output-format", "stream-json",
	"--verbose",
	"--permission-mode", "bypassPermissions",
}

// claudeToolGroups expands the shared harness-neutral tool-group vocabulary
// (see copilotToolGroups) into Claude Code's own built-in tool names. There is
// no Claude equivalent of the "github" group (Copilot resolves it to
// github-mcp-server-* tools) — a goober declaring tools: [github] on
// claude-code falls through expandToolGroup unexpanded, which is a known,
// separately-tracked gap (#1471 scope is tools, not that mapping).
var claudeToolGroups = map[string][]string{
	"shell": {
		"Bash", "Read", "Edit", "Write", "Glob", "Grep",
	},
	"telemetry": {
		"Read", "Glob", "Grep",
	},
}

// claudeAvailableTools expands a goober's declared tool allowlist into the
// deduplicated set of concrete Claude Code built-in tool names.
func claudeAvailableTools(tools []string) []string {
	var expanded []string
	seen := make(map[string]struct{})
	for _, declared := range tools {
		for _, tool := range expandToolGroup(declared, claudeToolGroups) {
			if _, ok := seen[tool]; ok {
				continue
			}
			seen[tool] = struct{}{}
			expanded = append(expanded, tool)
		}
	}
	return expanded
}

// claudeExtraArgs builds the CLI flags controlling tool availability. With no
// declared tools it returns defaultClaudeExtraArgs unchanged, preserving
// today's unrestricted-bypass behavior byte-for-byte. With a non-empty
// allowlist it uses --tools to define which built-in tools exist at all (the
// analog of Copilot's --available-tools) plus --allowedTools to pre-approve
// exactly that same set, so the session runs non-interactively without
// needing the blanket --permission-mode bypassPermissions escape hatch.
func claudeExtraArgs(tools []string) []string {
	if len(tools) == 0 {
		return append([]string(nil), defaultClaudeExtraArgs...)
	}
	allowlist := strings.Join(claudeAvailableTools(tools), ",")
	return []string{
		"--output-format", "stream-json",
		"--verbose",
		"--tools", allowlist,
		"--allowedTools", allowlist,
	}
}

// claudeKnownModels are the Claude Code model identifiers this adapter
// accepts. Unlike the Copilot CLI adapter, Claude Code exposes no
// authenticated "list available models" API to discover this set
// dynamically, so it is maintained here and rejected at config admission
// (ValidateConfig) — not at Run time, since Run may be driving a session
// whose configuration was already admitted, or a caller (a test, `goobers
// run`) that intentionally bypasses admission.
var claudeKnownModels = map[string]bool{
	"claude-fable-5":            true,
	"claude-opus-5":             true,
	"claude-sonnet-5":           true,
	"claude-haiku-4-5-20251001": true,
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
	// EphemeralTmp binds `tmp:ephemeral` on the self runner for this
	// adapter's subprocess — see CopilotAdapter.EphemeralTmp for the full
	// contract; the two adapters share it so a self entry declaring the
	// effect is true of every agentic stage, not just the ones that happen to
	// run under one harness.
	EphemeralTmp bool
	// EphemeralTmpRoot overrides the temp root the per-attempt directory is
	// carved out of. Empty means the daemon's own temp root.
	EphemeralTmpRoot string
}

// Name returns the adapter's diagnostic identity.
func (c *ClaudeAdapter) Name() string { return "claude-code" }

// ValidateConfig checks Claude Code model and harness option values. This is
// called during config admission, so an unsupported model is rejected before
// a run is ever attempted rather than failing (or silently misbehaving)
// mid-session.
func (c *ClaudeAdapter) ValidateConfig(model string, options map[string]apiextensionsv1.JSON) error {
	if err := validateClaudeModel(model); err != nil {
		return err
	}
	_, err := normalizeClaudeConfig(model, options)
	return err
}

func validateClaudeModel(model string) error {
	// "" and "auto" both mean "let the harness pick" — "auto" is the
	// cross-adapter sentinel goober configs use for this (see the Copilot
	// adapter's own always-valid "auto" entry), so it must be accepted here
	// too rather than rejected as an unknown model.
	if model == "" || model == "auto" || claudeKnownModels[model] {
		return nil
	}
	valid := make([]string, 0, len(claudeKnownModels))
	for name := range claudeKnownModels {
		valid = append(valid, name)
	}
	sort.Strings(valid)
	return fmt.Errorf("unknown model %q; valid models: %s", model, strings.Join(valid, ", "))
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

// buildClaudeArgv assembles the claude CLI invocation with every flag ahead
// of a "--" terminator and the prompt last. Every shipped instructions.md
// opens with YAML frontmatter, so the prompt routinely begins with "-"; a
// CLI parser that scans all argv positions for option-shaped tokens would
// otherwise misparse it regardless of where in argv it sits. Putting the
// prompt behind "--" as the final element is the only placement immune to
// that, since everything after "--" is taken as a literal. It returns the
// argv plus the indices of the prompt and the "--session-id" flag token,
// both of which the completion-recovery path rewrites in place.
func buildClaudeArgv(baseCommand, extra []string, model, effort, sessionID, prompt string) (argv []string, promptArg, sessionSelectorArg int) {
	argv = append(argv, baseCommand...)
	argv = append(argv, "-p")
	argv = append(argv, extra...)
	if model != "" {
		argv = append(argv, "--model", model)
	}
	if effort != "" {
		argv = append(argv, "--effort", effort)
	}
	sessionSelectorArg = len(argv)
	argv = append(argv, "--session-id", sessionID)
	argv = append(argv, "--")
	promptArg = len(argv)
	argv = append(argv, prompt)
	return argv, promptArg, sessionSelectorArg
}

func (c *ClaudeAdapter) runner() ProcessRunner {
	if c.Runner != nil {
		return c.Runner
	}
	return ExecProcessRunner{}
}

// Run executes one non-interactive Claude Code session.
func (c *ClaudeAdapter) Run(ctx context.Context, req RunRequest) (out Outcome, runErr error) {
	if err := validateStandardExecution(req); err != nil {
		return Outcome{}, err
	}
	if len(c.Command) == 0 {
		return Outcome{}, fmt.Errorf("harness: claude-code: no command configured")
	}
	if req.Workspace == "" {
		return Outcome{}, fmt.Errorf("harness: claude-code: RunRequest.Workspace is empty")
	}
	// The self-runner tmp:ephemeral binding for this attempt, reclaimed on
	// every exit path below.
	ephemeralTmp, err := establishEphemeralTmp(c.Name(), c.EphemeralTmp, c.EphemeralTmpRoot)
	if err != nil {
		return Outcome{}, err
	}
	defer func() { _ = ephemeralTmp.Reclaim() }()
	req = withAutoGoobersIOClaude(req, c.SelfBin)
	options, err := normalizeClaudeConfig(req.Model, req.HarnessOptions)
	if err != nil {
		return Outcome{}, fmt.Errorf("harness: claude-code: invalid configuration: %w", err)
	}
	if err := validateToolAllowlist(c.Name(), req.Tools); err != nil {
		return Outcome{}, err
	}

	prompt := renderPrompt(req)
	debugPath := filepath.Join(req.Workspace, ".goobers", "prompt.md")
	if err := os.MkdirAll(filepath.Dir(debugPath), 0o755); err != nil {
		return Outcome{}, fmt.Errorf("harness: claude-code: prepare prompt dir: %w", err)
	}
	if err := os.WriteFile(debugPath, []byte(prompt), 0o600); err != nil {
		return Outcome{}, fmt.Errorf("harness: claude-code: write prompt: %w", err)
	}

	baseCommand := resolveHarnessCommand(c.Command)
	extra := c.ExtraArgs
	if extra == nil {
		extra = claudeExtraArgs(req.Tools)
	}
	var mcpConfigArgs []string
	goobersIOArg, err := goobersIOClaudeMCPConfigArg(req, c.SelfBin)
	if err != nil {
		return Outcome{}, fmt.Errorf("harness: claude-code: %w", err)
	}
	if goobersIOArg != "" {
		mcpConfigArgs = append(mcpConfigArgs, goobersIOArg)
	}
	declaredMCPArg, mcpEnvAdditions, err := prepareClaudeMCP(ctx, req)
	if err != nil {
		return Outcome{}, err
	}
	if declaredMCPArg != "" {
		mcpConfigArgs = append(mcpConfigArgs, declaredMCPArg)
	}
	// A single --mcp-config flag takes multiple space-separated values
	// (confirmed live: repeated servers from separate values merge cleanly),
	// so goobers-io's registration and a goober's declared mcpServers coexist
	// under one flag invocation without conflicting.
	if len(mcpConfigArgs) > 0 {
		extra = append(append([]string(nil), extra...), "--mcp-config")
		extra = append(extra, mcpConfigArgs...)
		extra = append(extra, "--strict-mcp-config")
	}
	sessionID, err := newHarnessSessionID()
	if err != nil {
		return Outcome{}, fmt.Errorf("harness: claude-code: create session id: %w", err)
	}
	argv, promptArg, sessionSelectorArg := buildClaudeArgv(baseCommand, extra, req.Model, options["effort"], sessionID, prompt)

	env, err := buildCredentialEnv(ctx, credentialEnvConfig{
		adapterName:                    c.Name(),
		envCapabilities:                c.EnvCapabilities,
		optionalCredentialCapabilities: c.OptionalCredentialCapabilities,
		extraEnvAllowlist:              c.ExtraEnvAllowlist,
		instanceRoot:                   c.InstanceRoot,
		selfBin:                        c.SelfBin,
		ephemeralTmp:                   ephemeralTmp,
	}, req)
	if err != nil {
		return Outcome{}, err
	}
	env = append(env, mcpEnvAdditions...)
	env = dropForeignAnthropicAPIKey(env)

	// Isolate this run from the invoking user's ambient ~/.claude: an
	// unsandboxed run must not inherit the host's personal settings, hooks,
	// plugins, or MCP servers just because it happens to run on a machine
	// with a populated home directory (a goober's behavior must depend only
	// on what the DSL config declares). CLAUDE_CONFIG_DIR is redirected to a
	// fresh, per-run directory seeded with nothing but a copy of the stored
	// credentials (if any) unconditionally, for both sandboxed and
	// unsandboxed runs.
	configDir, tempDir, err := prepareClaudeRuntime(req.Workspace)
	if err != nil {
		return Outcome{}, fmt.Errorf("harness: claude-code: isolate ambient config: %w", err)
	}
	if err := seedClaudeCredentials(ctx, env, configDir); err != nil {
		return Outcome{}, fmt.Errorf("harness: claude-code: isolate ambient config: %w", err)
	}
	env = overrideEnv(env, "CLAUDE_CONFIG_DIR", configDir)
	env = overrideEnv(env, "TMPDIR", tempDir)

	if req.Sandbox != nil {
		writableRoots, err := gitWritableRoots(req.Workspace)
		if err != nil {
			return Outcome{}, fmt.Errorf("harness: claude-code: sandbox: %w", err)
		}
		wrapped, shift, err := confineArgv(req.Sandbox, argv, req.Workspace, writableRoots)
		if err != nil {
			return Outcome{}, fmt.Errorf("harness: claude-code: sandbox: %w", err)
		}
		argv = wrapped
		promptArg += shift
		sessionSelectorArg += shift
	}

	agentTelemetry, err := beginAdapterAgentTelemetry(
		req, "claude", req.Model, req.Model,
		requestedHarnessOption(req, "effort"), options["effort"],
	)
	if err != nil {
		return Outcome{}, fmt.Errorf("harness: claude-code: start agent telemetry: %w", err)
	}
	defer agentTelemetry.finish(&out, &runErr)

	runner := c.runner()
	started := time.Now()
	initialCapture := &claudeTerminalCapture{}
	captures := []*claudeTerminalCapture{initialCapture}
	result, processErr := runner.Run(ctx, ProcessRequest{
		Command:            argv,
		Dir:                req.Workspace,
		Env:                env,
		Timeout:            req.Timeout,
		MaxTranscriptBytes: req.MaxTranscriptBytes,
		StdoutCapture:      initialCapture,
	})
	runErr = processErr
	invocationResults := []ProcessResult{result}
	prompts := []string{prompt}
	var payload []byte
	var completionErr error
	if processErr == nil {
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

	out = Outcome{
		Transcript:             result.Transcript,
		TranscriptTruncated:    result.TranscriptTruncated,
		TranscriptDroppedBytes: result.TranscriptDroppedBytes,
		Stderr:                 result.Stderr,
	}
	receipts, receiptsCollected, receiptsErr := collectGoobersIOReceipts(req, c.SelfBin)
	out.InputInspectionReceipts = receipts
	out.InputInspectionReceiptsCollected = receiptsCollected
	if receiptsErr != nil {
		runErr = errors.Join(runErr, fmt.Errorf("read goobers-io input inspection receipts: %w", receiptsErr))
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
		// Surface registered-but-unusable MCP servers loudly (#3356): the
		// CLI's init event is the only place a failed server registration is
		// visible — the session otherwise proceeds silently without those
		// tools, and the resulting stage failure wears an unrelated costume.
		out.MCPServerFailures = claudeMCPServerFailures(req, native)
		if err := agentTelemetry.emit(projectAgentEvents(native.data, req)...); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("harness: claude-code: project agent telemetry: %w", err))
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

func prepareClaudeRuntime(workspace string) (string, string, error) {
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
			// "No such item" is the Keychain's spelling of the same state the
			// file branch above treats as optional: this machine simply has no
			// stored Claude Code login. Every other platform returns nil here
			// rather than failing, and so should darwin — otherwise the adapter
			// is unusable on any Mac where Claude Code was never signed in,
			// which includes every CI runner. Genuine failures (a locked
			// keychain, a denied prompt, security(1) missing) still fail
			// closed, since they leave the credential state unknown.
			if isKeychainItemNotFound(err) {
				return nil
			}
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

// keychainItemNotFoundExit is security(1)'s exit status for
// errSecItemNotFound — "The specified item could not be found in the keychain."
// It is a distinct status from its permission and I/O failures, which is what
// makes "absent" separable from "could not be determined" here.
const keychainItemNotFoundExit = 44

// isKeychainItemNotFound reports whether err is security(1) saying the item
// simply is not there, as opposed to any failure that leaves the credential
// state unknown. Matched on the exit status rather than the message so it does
// not depend on security(1)'s stderr wording.
func isKeychainItemNotFound(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == keychainItemNotFoundExit
}
