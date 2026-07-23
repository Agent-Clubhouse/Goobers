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
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/procenv"
	"github.com/goobers/goobers/internal/telemetry"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// defaultPromptFlag is the flag CopilotAdapter passes before the rendered
// prompt text when PromptFlag is unset — `-p`/`--prompt <text>`: "Execute a
// prompt in non-interactive mode (exits after completion)" per the real CLI's
// own --help, confirmed by a live invocation while building this adapter.
const defaultPromptFlag = "-p"

// defaultExtraArgs is used when ExtraArgs is nil. --allow-all-tools is
// REQUIRED for the real CLI's non-interactive mode — without it, a session
// blocks on an interactive permission prompt instead of exiting, which would
// hang until Timeout fires. Tool-level sandboxing (restricting which tools
// Copilot may use) is deferred to V1 (SEC-044); V0's capability enforcement
// is the credential-scoping this adapter already does via EnvCapabilities.
var defaultExtraArgs = []string{"--allow-all-tools", "--log-level", "error"}

type copilotModelCapabilities struct {
	longContext     bool
	reasoningEffort map[string]struct{}
}

func copilotEfforts(values ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}

var copilotModels = map[string]copilotModelCapabilities{
	"auto":                 {},
	"claude-fable-5":       {longContext: true, reasoningEffort: copilotEfforts("none", "low", "medium", "high", "xhigh", "max")},
	"claude-sonnet-5":      {longContext: true, reasoningEffort: copilotEfforts("none", "low", "medium", "high", "xhigh", "max")},
	"claude-sonnet-4.6":    {longContext: true, reasoningEffort: copilotEfforts("none", "low", "medium", "high", "max")},
	"claude-sonnet-4.5":    {},
	"claude-haiku-4.5":     {},
	"claude-opus-4.8-fast": {longContext: true, reasoningEffort: copilotEfforts("none", "low", "medium", "high", "xhigh", "max")},
	"claude-opus-4.8":      {longContext: true, reasoningEffort: copilotEfforts("none", "low", "medium", "high", "xhigh", "max")},
	"claude-opus-4.7":      {longContext: true, reasoningEffort: copilotEfforts("none", "low", "medium", "high", "xhigh", "max")},
	"claude-opus-4.6":      {longContext: true, reasoningEffort: copilotEfforts("none", "low", "medium", "high", "max")},
	"claude-opus-4.5":      {},
	"gpt-5.6-sol":          {longContext: true, reasoningEffort: copilotEfforts("none", "low", "medium", "high", "xhigh", "max")},
	"gpt-5.6-terra":        {longContext: true, reasoningEffort: copilotEfforts("none", "low", "medium", "high", "xhigh", "max")},
	"gpt-5.6-luna":         {longContext: true, reasoningEffort: copilotEfforts("none", "low", "medium", "high", "xhigh", "max")},
	"gpt-5.5":              {longContext: true, reasoningEffort: copilotEfforts("none", "low", "medium", "high", "xhigh")},
	"gpt-5.4":              {longContext: true, reasoningEffort: copilotEfforts("none", "low", "medium", "high", "xhigh")},
	"gpt-5.3-codex":        {reasoningEffort: copilotEfforts("none", "low", "medium", "high", "xhigh")},
	"gpt-5.4-mini":         {reasoningEffort: copilotEfforts("none", "low", "medium", "high", "xhigh")},
	"gpt-5-mini":           {reasoningEffort: copilotEfforts("none", "low", "medium", "high")},
	"gemini-3.1-pro-preview": {longContext: true,
		reasoningEffort: copilotEfforts("none", "low", "medium", "high")},
	"gemini-3.5-flash": {longContext: true,
		reasoningEffort: copilotEfforts("none", "minimal", "low", "medium", "high")},
	"kimi-k2.7-code":   {},
	"mai-code-1-flash": {},
}

// CopilotAdapter is the V0 harness adapter for the GitHub Copilot CLI
// (GBO-040): it renders the invocation envelope + goober instructions into a
// prompt, runs the CLI non-interactively in the stage workspace with only the
// granted capabilities' credentials materialized into its environment,
// captures supported native session events when available, enforces the
// timeout, and reads back the completion file the prompt instructed the CLI to
// write.
//
// The exact CLI invocation shape is configurable rather than hardcoded
// (Command/PromptFlag/ExtraArgs) so it can be tuned without touching this
// adapter's logic, but the defaults here are verified against a real,
// installed, signed-in Copilot CLI (1.0.71) — not guessed: `copilot -p
// "<text>" --allow-all-tools --log-level error` performs the task and exits,
// confirmed by TestCopilotAdapterLiveSmoke.
type CopilotAdapter struct {
	// Command is the base CLI invocation, e.g. []string{"copilot"}.
	Command []string
	// PromptFlag precedes the rendered prompt text in the built argv.
	// Defaults to "-p" if empty.
	PromptFlag string
	// ExtraArgs are appended after the prompt flag/text. Defaults to
	// defaultExtraArgs (--allow-all-tools, required for non-interactive
	// mode) when nil; pass an empty (non-nil) slice to opt out.
	ExtraArgs []string
	// EnvCapabilities maps a declared capability name to the environment
	// variable the CLI reads its credential from, e.g.
	// {"repo:push": "GH_TOKEN"}. Only capabilities present here — and
	// present in the invocation's declared+granted set — ever reach the
	// subprocess environment (capability enforcement, GBO-052).
	EnvCapabilities map[string]string
	// OptionalCredentialCapabilities names capabilities whose credential may
	// be omitted because the CLI can use an existing authenticated user session.
	// A configured grant still resolves and injects normally; only the absence
	// of a grant is tolerated. Other capabilities remain fail-closed.
	OptionalCredentialCapabilities map[string]bool
	// Runner executes the subprocess; defaults to ExecProcessRunner.
	Runner ProcessRunner
	// VersionArgs are the args used to preflight-check the CLI responds
	// (default {"--version"}).
	VersionArgs []string
	// AuthCheckArgs, if non-empty, are run as a second preflight probe after
	// VersionArgs to detect a signed-OUT CLI: `--version` succeeds even when the
	// user is not authenticated (GBO-011, #238), so a version check alone can't
	// catch a signed-out session — the failure would instead surface mid-run as
	// a burned agentic attempt. A non-zero exit (or runner error) from this probe
	// fails preflight with an actionable sign-in message. Empty by default: the
	// exact non-interactive auth/status invocation the real Copilot CLI offers is
	// wired at the composition root once confirmed, so a wrong guess can't
	// falsely refuse to start every agentic run.
	AuthCheckArgs []string
	// ExtraEnvAllowlist names additional ambient env vars carried into the
	// harness subprocess (and its preflight probes) on top of the built-in
	// procenv default-deny allowlist — the instance's RunnerConfig.EnvPassthrough
	// (#736), kept in lockstep with the executor's identical extension so a
	// toolchain env var a `dotnet`/`cargo` agentic stage needs is visible to the
	// harness too. Empty by default: the built-in allowlist, unchanged.
	ExtraEnvAllowlist []string
}

// Name returns the adapter's registry name.
func (c *CopilotAdapter) Name() string { return "copilot-cli" }

// ValidateConfig rejects model and option values the Copilot CLI adapter does
// not know how to express. This is called during config admission.
func (c *CopilotAdapter) ValidateConfig(model string, options map[string]apiextensionsv1.JSON) error {
	_, err := normalizeCopilotConfig(model, options)
	return err
}

func normalizeCopilotConfig(model string, options map[string]apiextensionsv1.JSON) (map[string]string, error) {
	var capabilities copilotModelCapabilities
	if model != "" {
		var ok bool
		capabilities, ok = copilotModels[model]
		if !ok {
			return nil, fmt.Errorf("unknown model %q", model)
		}
	}
	names := make([]string, 0, len(options))
	for name := range options {
		names = append(names, name)
	}
	sort.Strings(names)
	normalized := make(map[string]string, len(options))
	for _, name := range names {
		if name != "context" && name != "reasoningEffort" {
			return nil, fmt.Errorf("unknown harness option %q", name)
		}
		var value string
		if err := json.Unmarshal(options[name].Raw, &value); err != nil {
			return nil, fmt.Errorf("harness option %q must be a string: %w", name, err)
		}
		switch name {
		case "context":
			if value != "default" && value != "long_context" {
				return nil, fmt.Errorf("invalid context value %q", value)
			}
			if value == "long_context" {
				if model == "" {
					return nil, fmt.Errorf("context value %q requires an explicit model", value)
				}
				if !capabilities.longContext {
					return nil, fmt.Errorf("context value %q is not supported by model %q", value, model)
				}
			}
		case "reasoningEffort":
			if model == "" {
				return nil, fmt.Errorf("reasoningEffort requires an explicit model")
			}
			if _, ok := capabilities.reasoningEffort[value]; !ok {
				return nil, fmt.Errorf("reasoningEffort value %q is not supported by model %q", value, model)
			}
		}
		normalized[name] = value
	}
	return normalized, nil
}

// Preflight verifies the Copilot CLI binary is on PATH and responds to a
// version check, returning its reported version on success.
func (c *CopilotAdapter) Preflight(ctx context.Context) (PreflightInfo, error) {
	if len(c.Command) == 0 {
		return PreflightInfo{}, fmt.Errorf("harness: copilot-cli: no command configured")
	}
	bin := c.Command[0]
	if _, err := exec.LookPath(bin); err != nil {
		return PreflightInfo{}, fmt.Errorf("harness: copilot-cli: %q not found on PATH — install the GitHub Copilot CLI "+
			"and sign in before running agentic stages", bin)
	}
	args := c.VersionArgs
	if len(args) == 0 {
		args = []string{"--version"}
	}
	// Explicit baseEnv(), not the ProcessRequest zero value — since #122,
	// ExecProcessRunner treats a nil Env as NO environment (SEC-045
	// default-deny), so the version-check subprocess needs this passed
	// explicitly the same way Run's credentialEnv does.
	res, err := c.runner().Run(ctx, ProcessRequest{Command: append([]string{bin}, args...), Env: baseEnv(c.ExtraEnvAllowlist)})
	if err != nil {
		return PreflightInfo{}, fmt.Errorf("harness: copilot-cli: %q did not respond to %v: %w — check it is installed and signed in", bin, args, err)
	}
	if res.ExitCode != 0 {
		return PreflightInfo{}, fmt.Errorf("harness: copilot-cli: %q %v exited %d — check it is installed and signed in", bin, args, res.ExitCode)
	}
	version := firstOutputLine(res.Transcript)
	if version == "" {
		return PreflightInfo{}, fmt.Errorf("harness: copilot-cli: %q %v returned no version", bin, args)
	}
	// A signed-out CLI passes --version but can't do agentic work, so probe
	// authentication too when configured (GBO-011, #238) — catching it here at
	// startup rather than as a burned mid-run agentic attempt.
	if len(c.AuthCheckArgs) > 0 {
		command := resolveCopilotCommand(c.Command)
		res, err := c.runner().Run(ctx, ProcessRequest{Command: append(command, c.AuthCheckArgs...), Env: baseEnv(c.ExtraEnvAllowlist)})
		if err != nil {
			return PreflightInfo{}, fmt.Errorf("harness: copilot-cli: %q %v (sign-in check) failed: %w — run the Copilot CLI and sign in", bin, c.AuthCheckArgs, err)
		}
		if res.ExitCode != 0 {
			return PreflightInfo{}, fmt.Errorf("harness: copilot-cli: %q %v (sign-in check) exited %d — the CLI appears signed out; run the Copilot CLI and sign in", bin, c.AuthCheckArgs, res.ExitCode)
		}
	}
	return PreflightInfo{Version: version}, nil
}

func firstOutputLine(output []byte) string {
	for line := range strings.SplitSeq(string(output), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

func (c *CopilotAdapter) runner() ProcessRunner {
	if c.Runner != nil {
		return c.Runner
	}
	return ExecProcessRunner{}
}

// Run renders the prompt, runs the CLI non-interactively in req.Workspace with
// capability-scoped credentials in its environment, prefers converted native
// session events over the subprocess transcript when available, and reads back
// the completion file at req.CompletionPath.
func (c *CopilotAdapter) Run(ctx context.Context, req RunRequest) (Outcome, error) {
	if len(c.Command) == 0 {
		return Outcome{}, fmt.Errorf("harness: copilot-cli: no command configured")
	}
	if req.Workspace == "" {
		// exec.Cmd treats Dir == "" as "run in the daemon's own working
		// directory" — a silent, surprising fallback (#122) rather than the
		// fail-closed misconfiguration error an unset workspace should be.
		return Outcome{}, fmt.Errorf("harness: copilot-cli: RunRequest.Workspace is empty")
	}
	harnessOptions, err := normalizeCopilotConfig(req.Model, req.HarnessOptions)
	if err != nil {
		return Outcome{}, fmt.Errorf("harness: copilot-cli: invalid configuration: %w", err)
	}

	prompt := renderPrompt(req)
	// Also write the rendered prompt to the workspace for human debugging —
	// the CLI itself receives it inline (its -p/--prompt flag takes text,
	// not a file path).
	debugPath := filepath.Join(req.Workspace, ".goobers", "prompt.md")
	if err := os.MkdirAll(filepath.Dir(debugPath), 0o755); err != nil {
		return Outcome{}, fmt.Errorf("harness: copilot-cli: prepare prompt dir: %w", err)
	}
	if err := os.WriteFile(debugPath, []byte(prompt), 0o644); err != nil {
		return Outcome{}, fmt.Errorf("harness: copilot-cli: write prompt: %w", err)
	}

	flag := c.PromptFlag
	if flag == "" {
		flag = defaultPromptFlag
	}
	extra := c.ExtraArgs
	if extra == nil {
		extra = defaultExtraArgs
	}
	baseCommand := resolveCopilotCommand(c.Command)
	argv := append(baseCommand, flag, prompt)
	promptArg := len(baseCommand) + 1
	if req.Model != "" {
		argv = append(argv, "--model", req.Model)
	}
	if value, ok := harnessOptions["context"]; ok {
		argv = append(argv, "--context", value)
	}
	if value, ok := harnessOptions["reasoningEffort"]; ok {
		argv = append(argv, "--reasoning-effort", value)
	}
	argv = append(argv, extra...)

	env, err := c.credentialEnv(ctx, req)
	if err != nil {
		return Outcome{}, err
	}
	nativeTranscriptPath := ""
	if !copilotCommandSelectsSession(argv) {
		captureID, err := newCopilotCaptureID()
		if err != nil {
			return Outcome{}, fmt.Errorf("harness: copilot-cli: create transcript capture id: %w", err)
		}
		argv = append(argv, "--session-id", captureID)
		// Pin the log to this run without replacing the home that also holds
		// the user's Copilot configuration.
		if copilotHome, ok := copilotConfigHome(env); ok {
			nativeTranscriptPath = copilotSessionLogPath(copilotHome, captureID)
		}
	}

	runner := c.runner()
	started := time.Now()
	result, runErr := runner.Run(ctx, ProcessRequest{
		Command:            argv,
		Dir:                req.Workspace,
		Env:                env,
		Timeout:            req.Timeout,
		MaxTranscriptBytes: req.MaxTranscriptBytes,
	})
	var payload []byte
	var completionErr error
	if runErr == nil {
		payload, completionErr = readCompletion(req.Workspace, req.CompletionPath)
		if errors.Is(completionErr, ErrNoCompletion) {
			// A clean Copilot exit can still omit the contract file. Give the
			// same session one contract-only turn without extending its budget.
			totalTimeout := req.Timeout
			if totalTimeout <= 0 {
				totalTimeout = DefaultTimeout
			}
			remaining := totalTimeout - time.Since(started)
			if remaining <= 0 {
				runErr = fmt.Errorf("%w after %s: %s", ErrTimeout, totalTimeout, argv[0])
				completionErr = nil
			} else {
				recoveryArgv := append([]string(nil), argv...)
				recoveryArgv[promptArg] = renderCompletionRecoveryPrompt(req)
				recovery, err := runner.Run(ctx, ProcessRequest{
					Command:            recoveryArgv,
					Dir:                req.Workspace,
					Env:                env,
					Timeout:            remaining,
					MaxTranscriptBytes: req.MaxTranscriptBytes,
				})
				result = mergeProcessResults(result, recovery, req.MaxTranscriptBytes)
				if err != nil {
					runErr = err
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
	if nativeTranscriptPath != "" {
		if native, ok := readCopilotSessionTranscript(nativeTranscriptPath, req.MaxTranscriptBytes); ok {
			out.Metrics = native.metrics
			out.ModelUsage = native.modelUsage
			if len(native.data) > 0 {
				out.Transcript = native.data
				out.TranscriptSchema = telemetry.GenAIEventSchema
				out.TranscriptTruncated = native.truncated
				out.TranscriptDroppedBytes = native.droppedBytes
			}
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

func mergeProcessResults(first, second ProcessResult, limit int64) ProcessResult {
	if limit <= 0 {
		limit = DefaultMaxTranscriptBytes
	}

	firstTranscript := processTranscriptBytes(first)
	secondTranscript := processTranscriptBytes(second)

	// The recovery turn is the most useful diagnostic when the first turn
	// omitted its contract, so retain it first and use the remaining allowance
	// for the initial turn.
	secondRetained := min(int64(len(secondTranscript)), limit)
	remaining := limit - secondRetained
	var firstRetained int64
	if secondRetained == 0 {
		firstRetained = min(int64(len(firstTranscript)), remaining)
	} else if len(firstTranscript) > 0 && remaining > 1 {
		firstRetained = min(int64(len(firstTranscript)), remaining-1)
	}
	dropped := first.TranscriptDroppedBytes + second.TranscriptDroppedBytes +
		int64(len(firstTranscript)) - firstRetained +
		int64(len(secondTranscript)) - secondRetained

	transcript := append([]byte(nil), firstTranscript[:firstRetained]...)
	if firstRetained > 0 && secondRetained > 0 {
		transcript = append(transcript, '\n')
	}
	transcript = append(transcript, secondTranscript[:secondRetained]...)
	if dropped > 0 {
		transcript = append(transcript, transcriptTruncationMarker(dropped)...)
	}
	return ProcessResult{
		Transcript:             transcript,
		ExitCode:               second.ExitCode,
		TranscriptTruncated:    first.TranscriptTruncated || second.TranscriptTruncated || dropped > 0,
		TranscriptDroppedBytes: dropped,
	}
}

func processTranscriptBytes(result ProcessResult) []byte {
	if result.TranscriptDroppedBytes <= 0 {
		return result.Transcript
	}
	return bytes.TrimSuffix(result.Transcript, transcriptTruncationMarker(result.TranscriptDroppedBytes))
}

// baseEnv returns the minimal, explicit env every harness process starts
// with — internal/procenv.BaseEnvWith, the allowlist internal/executor's
// baseEnv() shares (#248, closing the #98/#122 drift for good: one
// definition instead of two hand-kept-in-sync copies). extra carries the
// instance-config-declared passthrough names (RunnerConfig.EnvPassthrough,
// #736), additively and still default-deny.
func baseEnv(extra []string) []string {
	return procenv.BaseEnvWith(extra)
}

// credentialEnv builds the subprocess environment: baseEnv() (PATH/HOME/
// TMPDIR — never a secret store, never the full os.Environ()), the stage
// telemetry directory, and exactly the capability tokens this adapter is
// configured to inject and that were actually declared for this invocation.
// A configured capability that fails to resolve is a hard stop — the harness
// never runs half-credentialed.
func (c *CopilotAdapter) credentialEnv(ctx context.Context, req RunRequest) ([]string, error) {
	env := baseEnv(c.ExtraEnvAllowlist)
	telemetryDir := req.TelemetryDir
	if telemetryDir == "" {
		telemetryDir = telemetry.PrepareStageTelemetryDir(req.Workspace)
	}
	if telemetryDir != "" {
		env = append(env, telemetry.StageTelemetryEnv+"="+telemetryDir)
	}
	for _, capability := range req.Envelope.Capabilities {
		envVar, ok := c.EnvCapabilities[capability]
		if !ok {
			continue
		}
		token, err := req.Credentials.Token(ctx, capability)
		if err != nil {
			if c.OptionalCredentialCapabilities[capability] &&
				errors.Is(err, credentials.ErrNoCredentialForCapability) {
				continue
			}
			return nil, fmt.Errorf("harness: copilot-cli: resolve %s: %w", capability, err)
		}
		env = append(env, envVar+"="+token)
	}
	return env, nil
}
