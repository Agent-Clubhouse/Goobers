package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/safepath"
	"github.com/goobers/goobers/internal/telemetry"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

const codexModelEnv = "CODEX_API_KEY"

// CodexAdapter drives the OpenAI Codex CLI in non-interactive exec mode.
type CodexAdapter struct {
	Command                        []string
	EnvCapabilities                map[string]string
	OptionalCredentialCapabilities map[string]bool
	Runner                         ProcessRunner
	ExtraEnvAllowlist              []string
	InstanceRoot                   string
	SelfBin                        string
	EphemeralTmp                   bool
	EphemeralTmpRoot               string
	ModelCredential                func(context.Context) (string, error)
}

// Name returns the Codex harness identifier.
func (c *CodexAdapter) Name() string { return "codex" }

func (c *CodexAdapter) runner() ProcessRunner {
	if c.Runner != nil {
		return c.Runner
	}
	return ExecProcessRunner{}
}

// ValidateConfig validates Codex model and harness options.
func (c *CodexAdapter) ValidateConfig(model string, options map[string]apiextensionsv1.JSON) error {
	_, err := normalizeCodexConfig(model, options)
	return err
}

func normalizeCodexConfig(model string, options map[string]apiextensionsv1.JSON) (map[string]string, error) {
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
		case "minimal", "low", "medium", "high", "xhigh":
		default:
			return nil, fmt.Errorf("invalid effort value %q", value)
		}
		normalized[name] = value
	}
	return normalized, nil
}

// Preflight verifies the Codex CLI and its configured API key.
func (c *CodexAdapter) Preflight(ctx context.Context) (PreflightInfo, error) {
	if len(c.Command) == 0 {
		return PreflightInfo{}, fmt.Errorf("harness: codex: no command configured")
	}
	bin := c.Command[0]
	if _, err := exec.LookPath(bin); err != nil {
		return PreflightInfo{}, fmt.Errorf("harness: codex: %q not found on PATH — install Codex CLI and sign in before running agentic stages", bin)
	}
	env := baseEnv(c.ExtraEnvAllowlist)
	hasAPIKey := false
	if c.ModelCredential != nil {
		token, err := c.ModelCredential(ctx)
		if err != nil {
			return PreflightInfo{}, fmt.Errorf("harness: codex: resolve agent:model credential: %w", err)
		}
		if isOpenAIAPIKey(token) {
			env = overrideEnv(env, codexModelEnv, token)
			hasAPIKey = true
		}
	} else {
		ambientAPIKey := strings.TrimSpace(os.Getenv(codexModelEnv))
		if isOpenAIAPIKey(ambientAPIKey) {
			env = overrideEnv(env, codexModelEnv, ambientAPIKey)
			hasAPIKey = true
		}
	}
	baseCommand := resolveHarnessCommand(c.Command)
	versionCommand := append(append([]string(nil), baseCommand...), "--version")
	res, err := c.runner().Run(ctx, ProcessRequest{
		Command:            versionCommand,
		Env:                env,
		MaxTranscriptBytes: maxPreflightDiagnosticBytes,
	})
	if err != nil || res.ExitCode != 0 {
		return PreflightInfo{}, preflightProbeError(
			fmt.Sprintf("harness: codex: %q --version", bin),
			res, err, "check that the CLI is installed",
		)
	}
	version := firstOutputLine(res.Transcript)
	if version == "" {
		return PreflightInfo{}, fmt.Errorf("harness: codex: %q --version returned no version", bin)
	}
	if hasAPIKey {
		return PreflightInfo{Version: version}, nil
	}
	return PreflightInfo{}, fmt.Errorf("harness: codex: configure an OpenAI API key for agent:model; stored Codex login is not used because workspace-write commands can read CODEX_HOME")
}

func isOpenAIAPIKey(value string) bool {
	return strings.HasPrefix(value, "sk-")
}

func buildCodexArgv(baseCommand []string, model, effort, workspace string, shellEnvNames []string) []string {
	trustedPath := filepath.Clean(workspace)
	argv := append([]string(nil), baseCommand...)
	argv = append(argv,
		"exec",
		"--json",
		"--ephemeral",
		"--ignore-rules",
		"--disable", "hooks",
		"--sandbox", "workspace-write",
		"--skip-git-repo-check",
		"-C", workspace,
		"-c", `projects.`+tomlQuote(trustedPath)+`.trust_level="untrusted"`,
		"-c", "project_root_markers=[]",
		"-c", `web_search="disabled"`,
		"-c", "sandbox_workspace_write.network_access=false",
		"-c", `shell_environment_policy.inherit="all"`,
		"-c", "shell_environment_policy.ignore_default_excludes=false",
		"-c", "shell_environment_policy.include_only="+tomlStringArray(shellEnvNames),
	)
	if model != "" && model != "auto" {
		argv = append(argv, "--model", model)
	}
	if effort != "" {
		argv = append(argv, "-c", `model_reasoning_effort="`+effort+`"`)
	}
	argv = append(argv, "-")
	return argv
}

type preparedCodexInvocation struct {
	req     RunRequest
	argv    []string
	env     []string
	prompt  string
	cleanup func()
}

// Run executes one Codex-backed agentic invocation.
func (c *CodexAdapter) Run(ctx context.Context, req RunRequest) (out Outcome, runErr error) {
	if err := validateStandardExecution(req); err != nil {
		return Outcome{}, err
	}
	if len(c.Command) == 0 {
		return Outcome{}, fmt.Errorf("harness: codex: no command configured")
	}
	if req.Workspace == "" {
		return Outcome{}, fmt.Errorf("harness: codex: RunRequest.Workspace is empty")
	}
	if len(req.Tools) > 0 {
		return Outcome{}, fmt.Errorf("harness: codex: restrictive tools declarations are unsupported because Codex CLI exposes no general built-in-tool allowlist")
	}
	options, err := normalizeCodexConfig(req.Model, req.HarnessOptions)
	if err != nil {
		return Outcome{}, fmt.Errorf("harness: codex: invalid configuration: %w", err)
	}

	prepared, err := c.prepareInvocation(ctx, req, options)
	if err != nil {
		return Outcome{}, err
	}
	defer prepared.cleanup()
	req = prepared.req
	argv := prepared.argv
	env := prepared.env
	prompt := prepared.prompt

	agentTelemetry, err := beginAdapterAgentTelemetry(
		req, "codex", req.Model, req.Model,
		requestedHarnessOption(req, "effort"), options["effort"],
	)
	if err != nil {
		return Outcome{}, fmt.Errorf("harness: codex: start agent telemetry: %w", err)
	}
	defer agentTelemetry.finish(&out, &runErr)
	defer func() {
		receipts, collected, err := collectGoobersIOReceipts(req, c.SelfBin)
		out.InputInspectionReceipts = receipts
		out.InputInspectionReceiptsCollected = collected
		if err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("read goobers-io input inspection receipts: %w", err))
		}
	}()

	started := time.Now()
	result, parsed, processErr := runCodexInvocation(
		ctx, c.runner(), req, argv, env, prompt, req.Timeout, 1, agentTelemetry.activityObserver(),
	)
	out = Outcome{
		Transcript:             result.Transcript,
		TranscriptTruncated:    result.TranscriptTruncated,
		TranscriptDroppedBytes: result.TranscriptDroppedBytes,
		Stderr:                 result.Stderr,
	}
	if processErr != nil {
		runErr = errors.Join(runErr, processErr)
		return out, runErr
	}
	out.Metrics = parsed.metrics

	payload, completionErr := readCompletion(req.Workspace, req.CompletionPath)
	if errors.Is(completionErr, ErrNoCompletion) {
		totalTimeout := req.Timeout
		if totalTimeout <= 0 {
			totalTimeout = DefaultTimeout
		}
		remaining := totalTimeout - time.Since(started)
		if remaining <= 0 {
			runErr = fmt.Errorf("%w after %s: %s", ErrTimeout, totalTimeout, argv[0])
			return out, runErr
		}
		recoveryPrompt := renderCompletionRecoveryPrompt(req)
		recovery, recoveryParsed, recoveryErr := runCodexInvocation(
			ctx, c.runner(), req, argv, env, recoveryPrompt, remaining, 2, agentTelemetry.activityObserver(),
		)
		mergeCodexMetrics(parsed.metrics, recoveryParsed.metrics)
		out.Metrics = parsed.metrics
		result = mergeProcessResults(result, recovery, req.MaxTranscriptBytes)
		out.Transcript = result.Transcript
		out.TranscriptTruncated = result.TranscriptTruncated
		out.TranscriptDroppedBytes = result.TranscriptDroppedBytes
		out.Stderr = result.Stderr
		if recoveryErr != nil {
			runErr = recoveryErr
			return out, runErr
		}
		payload, completionErr = readCompletion(req.Workspace, req.CompletionPath)
	}
	if completionErr != nil {
		runErr = completionErr
		return out, runErr
	}
	out.Payload = payload
	return out, nil
}

func (c *CodexAdapter) prepareInvocation(ctx context.Context, req RunRequest, options map[string]string) (prepared preparedCodexInvocation, err error) {
	ephemeralTmp, err := establishEphemeralTmp(c.Name(), c.EphemeralTmp, c.EphemeralTmpRoot)
	if err != nil {
		return preparedCodexInvocation{}, err
	}

	runtimeRoot, configDir, tempDir, err := prepareCodexRuntime(c.EphemeralTmpRoot)
	if err != nil {
		_ = ephemeralTmp.Reclaim()
		return preparedCodexInvocation{}, fmt.Errorf("harness: codex: isolate ambient config: %w", err)
	}
	cleanup := func() {
		_ = os.RemoveAll(runtimeRoot)
		_ = ephemeralTmp.Reclaim()
	}
	defer func() {
		if err != nil {
			cleanup()
		}
	}()
	req = withAutoGoobersIOCodex(req, c.SelfBin)
	prompt := renderPrompt(req)
	debugPath, err := safepath.Resolve(req.Workspace, filepath.Join(".goobers", "prompt.md"), true)
	if err != nil {
		return preparedCodexInvocation{}, fmt.Errorf("harness: codex: resolve prompt path: %w", err)
	}
	if err := os.WriteFile(debugPath, []byte(prompt), 0o600); err != nil {
		return preparedCodexInvocation{}, fmt.Errorf("harness: codex: write prompt: %w", err)
	}

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
		return preparedCodexInvocation{}, err
	}
	env = dropForeignCodexAPIKey(env)
	if !environmentContainsOpenAIKey(env) && c.ModelCredential != nil {
		token, credentialErr := c.ModelCredential(ctx)
		if credentialErr != nil {
			return preparedCodexInvocation{}, fmt.Errorf("harness: codex: resolve agent:model credential: %w", credentialErr)
		}
		if isOpenAIAPIKey(token) {
			env = overrideEnv(env, codexModelEnv, token)
		}
	}
	if !environmentContainsOpenAIKey(env) {
		return preparedCodexInvocation{}, fmt.Errorf("harness: codex: an OpenAI API key is required for agent:model; stored Codex login is not used because workspace-write commands can read CODEX_HOME")
	}
	reservedEnv := []string{codexModelEnv, "CODEX_HOME", "TMPDIR", "TEMP", "TMP"}
	for _, name := range c.EnvCapabilities {
		reservedEnv = append(reservedEnv, name)
	}
	env, mcpSecretEnv, err := prepareCodexMCP(ctx, req, configDir, c.SelfBin, env, reservedEnv)
	if err != nil {
		return preparedCodexInvocation{}, err
	}
	env = overrideEnv(env, "CODEX_HOME", configDir)
	env = overrideEnv(env, "TMPDIR", tempDir)
	env = overrideEnv(env, "TEMP", tempDir)
	env = overrideEnv(env, "TMP", tempDir)

	secretEnv := []string{codexModelEnv, "CODEX_HOME"}
	for _, name := range c.EnvCapabilities {
		secretEnv = append(secretEnv, name)
	}
	secretEnv = append(secretEnv, mcpSecretEnv...)
	shellEnvNames := codexShellEnvironmentNames(env, secretEnv)
	argv := buildCodexArgv(resolveStdioHarnessCommand(c.Command), req.Model, options["effort"], req.Workspace, shellEnvNames)
	if req.Sandbox != nil {
		writableRoots, err := gitWritableRoots(req.Workspace)
		if err != nil {
			return preparedCodexInvocation{}, fmt.Errorf("harness: codex: sandbox: %w", err)
		}
		writableRoots = append(writableRoots, runtimeRoot)
		argv, _, err = confineArgv(req.Sandbox, argv, req.Workspace, writableRoots)
		if err != nil {
			return preparedCodexInvocation{}, fmt.Errorf("harness: codex: sandbox: %w", err)
		}
	}
	return preparedCodexInvocation{req: req, argv: argv, env: env, prompt: prompt, cleanup: cleanup}, nil
}

func runCodexInvocation(
	ctx context.Context,
	runner ProcessRunner,
	req RunRequest,
	argv, env []string,
	prompt string,
	timeout time.Duration,
	checkpoint int,
	activity ActivityObserver,
) (ProcessResult, codexParseResult, error) {
	stdout := newCodexJSONLCapture(req.MaxTranscriptBytes)
	result, err := runner.Run(ctx, ProcessRequest{
		Command:                      argv,
		Stdin:                        []byte(prompt),
		Dir:                          req.Workspace,
		Env:                          env,
		Timeout:                      timeout,
		MaxTranscriptBytes:           req.MaxTranscriptBytes,
		StdoutCapture:                stdout,
		TranscriptCheckpoint:         req.processTranscriptCheckpoint(checkpoint),
		TranscriptCheckpointInterval: req.TranscriptCheckpointInterval,
		Activity:                     activity,
	})
	if err != nil {
		return result, codexParseResult{}, err
	}
	parsed, err := stdout.result()
	if !stdout.hasData() {
		parsed, err = parseCodexJSONL(result.Transcript)
	}
	if err != nil {
		return result, codexParseResult{}, err
	}
	if parsed.failed != "" {
		return result, parsed, fmt.Errorf("harness: codex: turn failed: %s", parsed.failed)
	}
	if !parsed.completed {
		return result, parsed, fmt.Errorf("harness: codex: process exited successfully without a turn.completed event")
	}
	return result, parsed, nil
}

func dropForeignCodexAPIKey(env []string) []string {
	filtered := env[:0:0]
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if ok && name == codexModelEnv && !isOpenAIAPIKey(value) {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func environmentContainsOpenAIKey(env []string) bool {
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if ok && name == codexModelEnv && isOpenAIAPIKey(value) {
			return true
		}
	}
	return false
}

func prepareCodexRuntime(root string) (string, string, string, error) {
	if root == "" {
		root = os.TempDir()
	}
	runtimeRoot, err := os.MkdirTemp(root, "goobers-codex-runtime-*")
	if err != nil {
		return "", "", "", err
	}
	configDir := filepath.Join(runtimeRoot, "home")
	tempDir := filepath.Join(runtimeRoot, "tmp")
	for _, dir := range []string{configDir, tempDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			_ = os.RemoveAll(runtimeRoot)
			return "", "", "", err
		}
	}
	return runtimeRoot, configDir, tempDir, nil
}

type codexParseResult struct {
	completed bool
	failed    string
	metrics   map[string]float64
}

func mergeCodexMetrics(destination, source map[string]float64) {
	for name, value := range source {
		destination[name] += value
	}
}

func tomlQuote(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func tomlStringArray(values []string) string {
	var out strings.Builder
	out.WriteByte('[')
	for i, value := range values {
		if i > 0 {
			out.WriteString(",")
		}
		out.WriteString(tomlQuote(value))
	}
	out.WriteByte(']')
	return out.String()
}

func codexShellEnvironmentNames(env, excluded []string) []string {
	deny := make(map[string]bool, len(excluded))
	for _, name := range excluded {
		deny[strings.ToUpper(name)] = true
	}
	var names []string
	seen := map[string]bool{}
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		normalized := strings.ToUpper(name)
		if !ok || name == "" || deny[normalized] || seen[normalized] {
			continue
		}
		seen[normalized] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

type codexJSONLCapture struct {
	mu      sync.Mutex
	pending []byte
	maxLine int
	parsed  codexParseResult
	err     error
	wrote   bool
}

func newCodexJSONLCapture(limit int64) *codexJSONLCapture {
	if limit <= 0 {
		limit = DefaultMaxTranscriptBytes
	}
	return &codexJSONLCapture{
		maxLine: int(limit),
		parsed:  codexParseResult{metrics: map[string]float64{}},
	}
}

func (c *codexJSONLCapture) Write(data []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return len(data), nil
	}
	c.wrote = c.wrote || len(data) > 0
	c.pending = append(c.pending, data...)
	if len(c.pending) > c.maxLine && !bytes.Contains(c.pending, []byte{'\n'}) {
		c.err = fmt.Errorf("harness: codex: JSONL event exceeded %d bytes", c.maxLine)
		c.pending = nil
		return len(data), nil
	}
	for {
		newline := bytes.IndexByte(c.pending, '\n')
		if newline < 0 {
			break
		}
		raw := bytes.TrimSpace(c.pending[:newline])
		c.pending = c.pending[newline+1:]
		if len(raw) > 0 {
			c.err = parseCodexEvent(raw, &c.parsed)
			if c.err != nil {
				break
			}
		}
	}
	return len(data), nil
}

func (c *codexJSONLCapture) hasData() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wrote
}

func (c *codexJSONLCapture) result() (codexParseResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return codexParseResult{}, c.err
	}
	if raw := bytes.TrimSpace(c.pending); len(raw) > 0 {
		if err := parseCodexEvent(raw, &c.parsed); err != nil {
			return codexParseResult{}, err
		}
		c.pending = nil
	}
	return c.parsed, nil
}

func parseCodexJSONL(data []byte) (codexParseResult, error) {
	result := codexParseResult{metrics: map[string]float64{}}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), int(DefaultMaxTranscriptBytes))
	line := 0
	for scanner.Scan() {
		line++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		if err := parseCodexEvent(raw, &result); err != nil {
			return codexParseResult{}, fmt.Errorf("harness: codex: parse JSONL event %d: %w", line, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return codexParseResult{}, fmt.Errorf("harness: codex: read JSONL events: %w", err)
	}
	return result, nil
}

func parseCodexEvent(raw []byte, result *codexParseResult) error {
	var event struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
		Usage struct {
			InputTokens  float64 `json:"input_tokens"`
			OutputTokens float64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return err
	}
	switch event.Type {
	case "turn.completed":
		result.completed = true
		result.metrics[telemetry.AttrGenAIUsageInputTokens] += event.Usage.InputTokens
		result.metrics[telemetry.AttrGenAIUsageOutputTokens] += event.Usage.OutputTokens
	case "turn.failed":
		result.failed = event.Error.Message
	case "error":
		result.failed = event.Message
	}
	return nil
}
